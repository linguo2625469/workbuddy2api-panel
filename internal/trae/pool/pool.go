// Package pool 账号池：内存索引 + 冷却/禁用状态机 + state.json 持久化。
// 挑选策略：healthy 账号中剩余积分最多者（SPEC §4.7）。
package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/trae/auth"
)

// CoolKind 冷却类型。
type CoolKind int

const (
	CoolPlan CoolKind = iota // 1005 plan 权益不足 → 12h 长冷却
	CoolSoft                 // 429/404 → 60s 短冷却（404 不累计 errCount）
	CoolErr                  // 连续错误 → 10m 中冷却
)

func (k CoolKind) String() string {
	switch k {
	case CoolPlan:
		return "plan_limit"
	case CoolSoft:
		return "soft_rate"
	case CoolErr:
		return "error_threshold"
	}
	return "unknown"
}

// Status 单个账号对外暴露的状态（脱敏，不含 token）。
type Status struct {
	UID      string    `json:"uid"`
	Nickname string    `json:"nickname,omitempty"`
	Credits  int64     `json:"credits"`
	Cooling  bool      `json:"cooling"`
	Until    time.Time `json:"until,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	// Disabled = session 失效硬禁用（需重登或换文件恢复）；Enabled = 软开关（用户可逆启停）。
	// 对外暴露：Disabled 与 Enabled 都为 false 才算可被 Pick（healthy）。
	Disabled bool `json:"disabled"`
	Enabled  bool `json:"enabled"`
	ErrCount int  `json:"err_count,omitempty"`
}

type entry struct {
	a        *auth.Auth
	credits  int64
	disabled bool // session dead 硬禁用
	enabled  bool // 用户软开关（默认 true），false 时 Pick 跳过
	reason   string
	until    time.Time
	errCount int
}

func (e *entry) healthy(now time.Time) bool {
	if e.disabled || !e.enabled {
		return false
	}
	if !e.until.IsZero() && now.Before(e.until) {
		return false
	}
	return true
}

// stateEntry state.json 单账号持久化条目。
type stateEntry struct {
	Credits  int64     `json:"credits"`
	Disabled bool      `json:"disabled"`
	Enabled  *bool     `json:"enabled,omitempty"` // 指针：旧文件缺省时按 true 处理，不写回脏值
	Reason   string    `json:"reason,omitempty"`
	Until    time.Time `json:"until,omitempty"`
}

// stateFile 持久化格式。
type stateFile struct {
	Accounts map[string]stateEntry `json:"accounts"`
}

// Pool 账号池。
type Pool struct {
	mu      sync.RWMutex
	byUID   map[string]*entry
	stateFp string
}

// New 构建池；stateFp 非空时尝试加载旧状态。
func New(stateFp string) *Pool {
	p := &Pool{byUID: map[string]*entry{}, stateFp: stateFp}
	if stateFp != "" {
		p.load()
	}
	return p
}

// Add 加入账号；已存在则保留原状态、更新凭证。
func (p *Pool) Add(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[a.UID]; ok {
		e.a = a // 保留 credits/cooling/enabled 状态
		return
	}
	p.byUID[a.UID] = &entry{a: a, enabled: true}
}

// SyncToDir 用最新扫描结果对齐池：新账号加入、消失的账号剔除（状态保留）。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	for _, a := range auths {
		seen[a.UID] = true
		if e, ok := p.byUID[a.UID]; ok {
			e.a = a
		} else {
			p.byUID[a.UID] = &entry{a: a, enabled: true}
		}
	}
	for uid := range p.byUID {
		if !seen[uid] {
			delete(p.byUID, uid)
		}
	}
}

// Remove 删除账号（仅清内存索引与 state.json 条目；auths/trae-{uid}.json 由调用方删）。
// 不存在返回 false，调用方据此决定 404。
func (p *Pool) Remove(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.byUID[uid]; !ok {
		return false
	}
	delete(p.byUID, uid)
	p.saveLocked()
	return true
}

// SetEnabled 切换账号软开关；不影响 disabled（session dead）状态。
// reason 仅在关闭时记录。不存在返回 false。
func (p *Pool) SetEnabled(uid string, enabled bool, reason string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.enabled = enabled
	if !enabled && reason != "" {
		e.reason = reason
	}
	if enabled {
		// 重新启用时清掉软关闭的 reason；disabled/cooling 不动
		if e.reason != "" && !e.disabled && e.until.IsZero() {
			e.reason = ""
		}
	}
	p.saveLocked()
	return true
}

// Pick 返回 healthy 中积分最高的账号；无可用返回 nil。
func (p *Pool) Pick() *auth.Auth {
	return p.PickExcluding(nil)
}

// HealthyCount 返回当前健康（可受理请求）的账号数。
// 供统一网关做跨池兜底判定：0 = trae 池整体不可用。
func (p *Pool) HealthyCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	n := 0
	for _, e := range p.byUID {
		if e.healthy(now) {
			n++
		}
	}
	return n
}

// PickExcluding 同上，但跳过 tried 中的 uid（请求级轮换）。
func (p *Pool) PickExcluding(tried map[string]bool) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	var best *entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		if best == nil || e.credits > best.credits {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	return best.a
}

// SetCredits 更新账号积分。
func (p *Pool) SetCredits(uid string, credits int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = credits
	}
	p.saveLocked()
}

// Cooldown 冷却账号至 now+d。
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Now().Add(d)
		e.reason = reason
		e.errCount = 0
	}
	p.saveLocked()
}

// Disable 永久禁用（session 失效），需人工重登后手工恢复或文件替换。
func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = true
		e.reason = reason
	}
	p.saveLocked()
}

// ReenableIfCredits 签到后解冻：仅当 remain > 0 且账号处于冷却（非禁用）时恢复。
func (p *Pool) ReenableIfCredits(uid string, remain int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = remain
		if remain > 0 && !e.disabled {
			e.until = time.Time{}
			e.reason = ""
			e.errCount = 0
		}
	}
	p.saveLocked()
}

// NoteError 记录一次错误；达到 threshold 自动冷却 d 时长。
func (p *Pool) NoteError(uid string, threshold int, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount++
		if e.errCount >= threshold {
			e.until = time.Now().Add(d)
			e.reason = "consecutive errors"
			e.errCount = 0
		}
	}
	p.saveLocked()
}

// NoteSuccess 成功请求重置错误计数。
func (p *Pool) NoteSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount = 0
	}
}

// Status 查询单账号状态。
func (p *Pool) Status(uid string) (Status, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return Status{}, false
	}
	return p.statusOf(uid, e), true
}

// AuthByUID 返回账号的完整凭证（给调度器/运维接口用）。
func (p *Pool) AuthByUID(uid string) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.a
	}
	return nil
}

// List 返回所有账号状态（按 UID 排序，稳定输出）。
func (p *Pool) List() []Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := make([]Status, 0, len(uids))
	for _, uid := range uids {
		out = append(out, p.statusOf(uid, p.byUID[uid]))
	}
	return out
}

func (p *Pool) statusOf(uid string, e *entry) Status {
	now := time.Now()
	return Status{
		UID:      uid,
		Nickname: e.a.Nickname,
		Credits:  e.credits,
		Cooling:  !e.until.IsZero() && now.Before(e.until),
		Until:    e.until,
		Reason:   e.reason,
		Disabled: e.disabled,
		Enabled:  e.enabled,
		ErrCount: e.errCount,
	}
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------

func (p *Pool) load() {
	raw, err := os.ReadFile(p.stateFp)
	if err != nil {
		return
	}
	var sf stateFile
	if json.Unmarshal(raw, &sf) != nil {
		return
	}
	for uid, s := range sf.Accounts {
		enabled := true // 旧文件无 enabled 字段 → 默认启用，向后兼容
		if s.Enabled != nil {
			enabled = *s.Enabled
		}
		p.byUID[uid] = &entry{
			a:        &auth.Auth{UID: uid}, // placeholder，Add 时会换成完整凭证
			credits:  s.Credits,
			disabled: s.Disabled,
			enabled:  enabled,
			reason:   s.Reason,
			until:    s.Until,
		}
	}
}

func (p *Pool) saveLocked() {
	if p.stateFp == "" {
		return
	}
	sf := stateFile{Accounts: map[string]stateEntry{}}
	for uid, e := range p.byUID {
		se := stateEntry{
			Credits:  e.credits,
			Disabled: e.disabled,
			Reason:   e.reason,
			Until:    e.until,
		}
		// 仅在软关闭时写 enabled=false；默认 true 用 omitempty 省略，旧版本读为 true。
		if !e.enabled {
			f := false
			se.Enabled = &f
		}
		sf.Accounts[uid] = se
	}
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(p.stateFp); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := p.stateFp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p.stateFp)
}
