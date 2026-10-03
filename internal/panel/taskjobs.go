package panel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

const (
	maxTaskJobs       = 256
	maxTaskJobResults = 64
)

var (
	errTaskJobsNotStarted = errors.New("后台任务服务尚未启动")
	errTaskJobsStopped    = errors.New("后台任务服务正在关闭")
	errTaskJobAccount     = errors.New("account not found")
	errTaskJobGlobal      = errors.New(globalTaskWriteMessage)
	errTaskJobBusy        = errors.New("该账号有任务动作正在执行中，请等本轮结束后再试")
	errTaskJobDisabled    = errors.New("自动成长任务未启用")
	errTaskJobLimit       = errors.New("后台任务记录已满，且当前没有可清理的已完成记录")
)

// TaskJob 是账号全量自动任务的可查询状态。Results 沿用单项执行器的
// resultMap 字段，status 仅描述后台作业生命周期，不替代逐项执行结果。
type TaskJob struct {
	ID          string           `json:"id"`
	UID         string           `json:"uid"`
	Nickname    string           `json:"nickname"`
	Realm       string           `json:"realm"`
	Trigger     string           `json:"trigger"`
	Status      string           `json:"status"` // pending | running | finished | interrupted | error
	StartedAt   time.Time        `json:"started_at"`
	UpdatedAt   time.Time        `json:"updated_at"`
	FinishedAt  *time.Time       `json:"finished_at,omitempty"`
	CurrentTask string           `json:"current_task,omitempty"`
	Completed   int              `json:"completed"`
	Total       int              `json:"total"`
	Results     []map[string]any `json:"results"`
	Message     string           `json:"message,omitempty"`
	Error       string           `json:"error,omitempty"`
}

type taskJobFile struct {
	Version int       `json:"version"`
	Jobs    []TaskJob `json:"jobs"`
}

type taskJobManager struct {
	panel *Panel

	mu      sync.Mutex
	path    string
	jobs    map[string]TaskJob // latest job per uid
	active  map[string]string  // uid -> active job id
	ctx     context.Context
	cancel  context.CancelFunc
	sem     chan struct{}
	wg      sync.WaitGroup
	closed  bool
	stopOne sync.Once
}

// StartTaskJobs 加载账号后台任务状态，并绑定其 goroutine 到应用生命周期。
// path 应是与 state_file 同目录的 task-jobs.json 完整路径。
func (p *Panel) StartTaskJobs(ctx context.Context, path string) error {
	if p == nil {
		return errors.New("panel is nil")
	}
	if path == "" {
		return errors.New("task jobs path is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	p.taskMu.Lock()
	defer p.taskMu.Unlock()
	if p.taskJobs != nil {
		if filepath.Clean(p.taskJobs.path) != filepath.Clean(path) {
			return errors.New("task jobs already started with a different path")
		}
		return nil
	}

	jobs, err := readTaskJobs(path)
	if err != nil {
		return err
	}
	jobCtx, cancel := context.WithCancel(ctx)
	m := &taskJobManager{
		panel:  p,
		path:   path,
		jobs:   jobs,
		active: make(map[string]string),
		ctx:    jobCtx,
		cancel: cancel,
		sem:    make(chan struct{}, 2),
	}
	now := time.Now().UTC()
	changed := false
	for uid, job := range m.jobs {
		if job.Status == "pending" || job.Status == "running" {
			job.Status = "interrupted"
			job.Message = "进程上次退出时任务未完成，等待恢复"
			job.Error = ""
			job.CurrentTask = ""
			job.UpdatedAt = now
			job.FinishedAt = timePtr(now)
			m.jobs[uid] = job
			changed = true
		}
	}
	if changed {
		if err := m.persistLocked(); err != nil {
			cancel()
			return fmt.Errorf("save recovered task jobs: %w", err)
		}
	}
	p.taskJobs = m
	go func() {
		<-jobCtx.Done()
		m.stop()
	}()
	return nil
}

// StopTaskJobs 取消待执行作业并等待当前不可取消的上游调用返回。
// 返回后不再有后台任务 goroutine 写状态文件。
func (p *Panel) StopTaskJobs() {
	if p == nil {
		return
	}
	p.taskMu.Lock()
	m := p.taskJobs
	p.taskMu.Unlock()
	if m != nil {
		m.stop()
	}
}

func (m *taskJobManager) stop() {
	m.stopOne.Do(func() {
		m.mu.Lock()
		m.closed = true
		m.cancel()
		m.mu.Unlock()

		// runAutoAllObserved checks cancellation between phases/items. The current
		// upstream call may complete, but its actual result is recorded before Wait returns.
		m.wg.Wait()

		m.mu.Lock()
		now := time.Now().UTC()
		for uid, job := range m.jobs {
			if job.Status == "pending" || job.Status == "running" {
				job.Status = "interrupted"
				job.Message = "服务关闭，任务已中断"
				job.Error = ""
				job.CurrentTask = ""
				job.UpdatedAt = now
				job.FinishedAt = timePtr(now)
				m.jobs[uid] = job
			}
		}
		if err := m.persistLocked(); err != nil {
			log.Printf("panel: 保存后台任务中断状态失败: %v", err)
		}
		m.mu.Unlock()
	})
}

// StartAccountTaskJob 创建作业并立即返回。重复启动同一账号时返回现有活动作业，
// 已在单项动作或队列中执行则返回 errTaskJobBusy。
func (p *Panel) StartAccountTaskJob(uid, trigger string) (TaskJob, bool, error) {
	if uid == "" {
		return TaskJob{}, false, errTaskJobAccount
	}
	if trigger == "" {
		trigger = "manual"
	}
	if trigger != "manual" && (p.cfg.AutoTasksEnabled == nil || !p.cfg.AutoTasksEnabled()) {
		return TaskJob{}, false, errTaskJobDisabled
	}
	if p.cfg.Pool == nil {
		return TaskJob{}, false, errTaskJobAccount
	}
	a := p.cfg.Pool.AuthByUID(uid) // 只读本地账号状态；HTTP handler 不发起上游请求。
	if a == nil {
		return TaskJob{}, false, errTaskJobAccount
	}
	if a.IsGlobal() {
		return TaskJob{}, false, errTaskJobGlobal
	}
	m := p.getTaskJobManager()
	if m == nil {
		return TaskJob{}, false, errTaskJobsNotStarted
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx.Err() != nil {
		return TaskJob{}, false, errTaskJobsStopped
	}
	if _, active := m.active[uid]; active {
		job, ok := m.jobs[uid]
		if ok {
			return cloneTaskJob(job), false, nil
		}
	}

	jobID, err := newTaskJobID()
	if err != nil {
		return TaskJob{}, false, fmt.Errorf("create task job id: %w", err)
	}
	if old, exists := m.jobs[uid]; exists && (old.Status == "pending" || old.Status == "running") {
		// Active 状态始终应在 active map 中；防止损坏状态被静默覆盖。
		return cloneTaskJob(old), false, nil
	}
	if !p.tryLockAccount(uid) {
		return TaskJob{}, false, errTaskJobBusy
	}
	var evictedUID string
	var evictedJob TaskJob
	if _, exists := m.jobs[uid]; !exists && len(m.jobs) >= maxTaskJobs {
		evictedUID, evictedJob = m.evictOldestLocked()
		if evictedUID == "" {
			p.unlockAccount(uid)
			return TaskJob{}, false, errTaskJobLimit
		}
	}

	now := time.Now().UTC()
	job := TaskJob{
		ID:          jobID,
		UID:         uid,
		Nickname:    a.Nickname,
		Realm:       a.Realm(),
		Trigger:     trigger,
		Status:      "pending",
		StartedAt:   now,
		UpdatedAt:   now,
		CurrentTask: "等待后台执行",
		Total:       len(autoActions),
		Results:     []map[string]any{},
		Message:     "任务已排队",
	}
	previous, hadPrevious := m.jobs[uid]
	m.jobs[uid] = job
	m.active[uid] = job.ID
	if err := m.persistLocked(); err != nil {
		delete(m.active, uid)
		if hadPrevious {
			m.jobs[uid] = previous
		} else {
			delete(m.jobs, uid)
		}
		if evictedUID != "" {
			m.jobs[evictedUID] = evictedJob
		}
		p.unlockAccount(uid)
		return TaskJob{}, false, fmt.Errorf("persist task job: %w", err)
	}
	m.wg.Add(1)
	go m.run(uid, job.ID, a)
	return cloneTaskJob(job), true, nil
}

// StartNewAccountTasks 在配置开关开启时自动排入新国区账号的任务。
func (p *Panel) StartNewAccountTasks(uid string) {
	if p == nil || p.cfg.AutoTasksEnabled == nil || !p.cfg.AutoTasksEnabled() {
		return
	}
	if p.cfg.Pool == nil {
		return
	}
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil || a.IsGlobal() {
		return
	}
	if _, started, err := p.StartAccountTaskJob(uid, "new_account"); err != nil {
		log.Printf("panel: 新账号后台任务 uid=%s 启动失败: %v", uid, err)
	} else if started {
		log.Printf("panel: 新账号后台任务 uid=%s 已排队", uid)
	}
}

// ResumeTaskJobs 恢复上次进程退出时未完成的作业。任务执行器先回读服务端状态，
// 已领奖项目会在 executeAutoTask 内跳过动作，不会重复领奖或消耗对话。
func (p *Panel) ResumeTaskJobs() {
	m := p.getTaskJobManager()
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx.Err() != nil {
		return
	}
	var uids []string
	for uid, job := range m.jobs {
		if job.Status == "interrupted" {
			uids = append(uids, uid)
		}
	}
	sort.Slice(uids, func(i, j int) bool {
		return m.jobs[uids[i]].UpdatedAt.Before(m.jobs[uids[j]].UpdatedAt)
	})
	for _, uid := range uids {
		job := m.jobs[uid]
		if job.Trigger != "manual" && (p.cfg.AutoTasksEnabled == nil || !p.cfg.AutoTasksEnabled()) {
			continue
		}
		if p.cfg.Pool == nil {
			continue
		}
		a := p.cfg.Pool.AuthByUID(uid)
		if a == nil || a.IsGlobal() || accountTaskUIDDisabled(p.cfg.Pool, uid) {
			continue
		}
		if !p.tryLockAccount(uid) {
			// 其他任务当前持有共享账号锁；保持 interrupted，后续重启仍可恢复。
			continue
		}
		job.Status = "pending"
		job.CurrentTask = "等待后台恢复"
		job.Message = "正在恢复未完成任务"
		job.Error = ""
		job.UpdatedAt = time.Now().UTC()
		job.FinishedAt = nil
		m.jobs[uid] = job
		m.active[uid] = job.ID
		m.wg.Add(1)
		go m.run(uid, job.ID, a)
	}
	if err := m.persistLocked(); err != nil {
		log.Printf("panel: 保存后台任务恢复状态失败: %v", err)
	}
}

// ListTaskJobs 返回每个账号最新的作业，按更新时间从新到旧排列。
func (p *Panel) ListTaskJobs() []TaskJob {
	m := p.getTaskJobManager()
	if m == nil {
		return []TaskJob{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]TaskJob, 0, len(m.jobs))
	for _, job := range m.jobs {
		out = append(out, cloneTaskJob(job))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

// GetTaskJob 返回 uid 最新一条后台作业。
func (p *Panel) GetTaskJob(uid string) (TaskJob, bool) {
	m := p.getTaskJobManager()
	if m == nil {
		return TaskJob{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[uid]
	return cloneTaskJob(job), ok
}

func (p *Panel) taskJobsHandler(w http.ResponseWriter, r *http.Request) {
	autoEnabled := p.cfg.AutoTasksEnabled != nil && p.cfg.AutoTasksEnabled()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "auto_enabled": autoEnabled, "jobs": p.ListTaskJobs()})
}

func (p *Panel) taskJobStatus(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if p.cfg.Pool == nil || p.cfg.Pool.AuthByUID(uid) == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	job, ok := p.GetTaskJob(uid)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job": job})
}

func writeTaskJobError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, errTaskJobAccount):
		status = http.StatusNotFound
	case errors.Is(err, errTaskJobGlobal):
		status = http.StatusNotImplemented
	case errors.Is(err, errTaskJobBusy):
		status = http.StatusConflict
	case errors.Is(err, errTaskJobDisabled):
		status = http.StatusForbidden
	case errors.Is(err, errTaskJobsNotStarted), errors.Is(err, errTaskJobsStopped):
		status = http.StatusServiceUnavailable
	}
	writeErr(w, status, err.Error())
}

func (p *Panel) getTaskJobManager() *taskJobManager {
	if p == nil {
		return nil
	}
	p.taskMu.Lock()
	m := p.taskJobs
	p.taskMu.Unlock()
	return m
}

func (m *taskJobManager) run(uid, jobID string, a *auth.Auth) {
	defer m.wg.Done()
	defer m.panel.unlockAccount(uid)
	defer func() {
		if recovered := recover(); recovered != nil {
			m.finish(uid, jobID, "error", fmt.Sprintf("后台任务异常: %v", recovered))
		}
	}()

	select {
	case m.sem <- struct{}{}:
		defer func() { <-m.sem }()
	case <-m.ctx.Done():
		m.finish(uid, jobID, "interrupted", "服务关闭，任务已中断")
		return
	}
	if m.ctx.Err() != nil {
		m.finish(uid, jobID, "interrupted", "服务关闭，任务已中断")
		return
	}
	if m.panel.cfg.Pool == nil || m.panel.cfg.Pool.AuthByUID(uid) == nil || accountTaskUIDDisabled(m.panel.cfg.Pool, uid) {
		m.finish(uid, jobID, "error", "账号已删除或禁用，任务未启动")
		return
	}
	// 等待执行期间可能重新登录过，开始时采用池中的最新凭证。
	a = m.panel.cfg.Pool.AuthByUID(uid)
	if a == nil || a.IsGlobal() {
		m.finish(uid, jobID, "error", "账号不存在或不支持国区自动任务")
		return
	}
	m.mu.Lock()
	job, ok := m.jobs[uid]
	if !ok || job.ID != jobID {
		m.mu.Unlock()
		return
	}
	job.Status = "running"
	job.CurrentTask = "读取服务端任务状态"
	job.Message = "后台任务正在执行"
	job.UpdatedAt = time.Now().UTC()
	m.jobs[uid] = job
	if err := m.persistLocked(); err != nil {
		log.Printf("panel: 保存后台任务运行状态 uid=%s: %v", uid, err)
	}
	m.mu.Unlock()

	jobCtx, cancelJob := context.WithCancel(m.ctx)
	defer cancelJob()
	stopReason := ""
	m.panel.runAutoAllObserved(jobCtx, a, func(event autoTaskRunEvent) {
		m.observe(uid, jobID, event)
		current := m.panel.cfg.Pool.AuthByUID(uid)
		if current == nil || accountTaskUIDDisabled(m.panel.cfg.Pool, uid) {
			stopReason = "账号已移除或停用，后台任务已停止"
			cancelJob()
		} else if current != a {
			stopReason = "账号凭证已更新，请重新启动后台任务"
			cancelJob()
		}
	})
	if stopReason != "" {
		m.finish(uid, jobID, "interrupted", stopReason)
	} else if jobCtx.Err() != nil {
		m.finish(uid, jobID, "interrupted", "服务关闭，任务已中断")
	} else {
		m.finish(uid, jobID, "finished", "全部可自动任务已处理")
	}
}

func (m *taskJobManager) observe(uid, jobID string, event autoTaskRunEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[uid]
	if !ok || job.ID != jobID {
		return
	}
	switch event.Kind {
	case "phase", "task_start":
		job.CurrentTask = event.TaskCode
		job.Message = "后台任务正在执行"
	case "result":
		if event.Result != nil {
			upsertTaskJobResult(&job, event.TaskCode, event.Result)
		}
		if event.Completed {
			job.Completed = countCompletedTaskResults(job.Results)
		}
		job.CurrentTask = ""
	}
	job.UpdatedAt = time.Now().UTC()
	m.jobs[uid] = job
	if err := m.persistLocked(); err != nil {
		log.Printf("panel: 保存后台任务进度 uid=%s: %v", uid, err)
	}
}

func (m *taskJobManager) finish(uid, jobID, status, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[uid]
	if !ok || job.ID != jobID {
		return
	}
	job.Status = status
	job.Message = message
	job.CurrentTask = ""
	job.UpdatedAt = time.Now().UTC()
	job.FinishedAt = timePtr(job.UpdatedAt)
	if status == "error" {
		job.Error = message
	} else {
		job.Error = ""
	}
	m.jobs[uid] = job
	delete(m.active, uid)
	if err := m.persistLocked(); err != nil {
		log.Printf("panel: 保存后台任务结束状态 uid=%s: %v", uid, err)
	}
}

func (m *taskJobManager) evictOldestLocked() (string, TaskJob) {
	var candidate string
	var oldest time.Time
	for uid, job := range m.jobs {
		if _, active := m.active[uid]; active || job.Status == "pending" || job.Status == "running" {
			continue
		}
		if candidate == "" || job.UpdatedAt.Before(oldest) {
			candidate, oldest = uid, job.UpdatedAt
		}
	}
	if candidate == "" {
		return "", TaskJob{}
	}
	job := m.jobs[candidate]
	delete(m.jobs, candidate)
	return candidate, job
}

func (m *taskJobManager) persistLocked() error {
	jobs := make([]TaskJob, 0, len(m.jobs))
	for _, job := range m.jobs {
		jobs = append(jobs, job)
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].UpdatedAt.After(jobs[j].UpdatedAt) })
	if len(jobs) > maxTaskJobs {
		jobs = jobs[:maxTaskJobs]
	}
	raw, err := json.MarshalIndent(taskJobFile{Version: 1, Jobs: jobs}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(m.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".task-jobs-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, m.path); err != nil {
		return err
	}
	if err := os.Chmod(m.path, 0o600); err != nil {
		return err
	}
	return nil
}

func readTaskJobs(path string) (map[string]TaskJob, error) {
	jobs := make(map[string]TaskJob)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return jobs, nil
	}
	if err != nil {
		return nil, err
	}
	var file taskJobFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("decode task jobs: %w", err)
	}
	for _, job := range file.Jobs {
		if job.UID == "" || job.ID == "" {
			continue
		}
		if len(job.Results) > maxTaskJobResults {
			job.Results = job.Results[len(job.Results)-maxTaskJobResults:]
		}
		jobs[job.UID] = job
	}
	if len(jobs) > maxTaskJobs {
		ordered := make([]TaskJob, 0, len(jobs))
		for _, job := range jobs {
			ordered = append(ordered, job)
		}
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].UpdatedAt.After(ordered[j].UpdatedAt) })
		jobs = make(map[string]TaskJob, maxTaskJobs)
		for _, job := range ordered[:maxTaskJobs] {
			jobs[job.UID] = job
		}
	}
	return jobs, nil
}

func newTaskJobID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

func upsertTaskJobResult(job *TaskJob, taskCode string, result map[string]any) {
	if job == nil || result == nil {
		return
	}
	if taskCode == "" {
		if code, ok := result["task_code"].(string); ok {
			taskCode = code
		}
	}
	for i := range job.Results {
		if code, ok := job.Results[i]["task_code"].(string); ok && code == taskCode {
			job.Results[i] = cloneResultMap(result)
			return
		}
	}
	if len(job.Results) >= maxTaskJobResults {
		return
	}
	job.Results = append(job.Results, cloneResultMap(result))
}

func countCompletedTaskResults(results []map[string]any) int {
	known := make(map[string]struct{}, len(autoActions))
	for _, result := range results {
		code, _ := result["task_code"].(string)
		if code == "" {
			continue
		}
		if autoActionFor(code) != nil {
			known[code] = struct{}{}
		}
	}
	return len(known)
}

func cloneTaskJob(job TaskJob) TaskJob {
	results := job.Results
	job.Results = make([]map[string]any, len(results))
	for i, result := range results {
		job.Results[i] = cloneResultMap(result)
	}
	if job.FinishedAt != nil {
		job.FinishedAt = timePtr(*job.FinishedAt)
	}
	return job
}

func cloneResultMap(result map[string]any) map[string]any {
	if result == nil {
		return nil
	}
	copy := make(map[string]any, len(result))
	for key, value := range result {
		copy[key] = value
	}
	return copy
}

func timePtr(value time.Time) *time.Time { return &value }

func accountTaskUIDDisabled(p *pool.Pool, uid string) bool {
	for _, account := range p.List() {
		if account.UID == uid {
			return account.Disabled
		}
	}
	return true
}
