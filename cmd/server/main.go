// main.go workbuddy2api 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/panel"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/redisstore"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/server"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// appVersion 网关版本（fork 版：面板 + 任务体系），透出到 /panel/api/overview。
// 用 var 而非常量：Release workflow 以 -ldflags "-X main.appVersion=<tag>"
// 注入 git tag，本地构建回落此处默认值。
var appVersion = "1.11.2-panel"

// usagePathFor 由 state 文件路径推出用量文件路径：同目录、文件名 usage.json。
// 这样 config 里改 state_file 时用量数据跟着走，不需要额外配置项。
func usagePathFor(stateFile string) string { return stateSibling(stateFile, "usage.json") }

// stateSibling 返回与 state 文件同目录的指定文件名路径（相对路径场景回落当前目录）。
// usage.json（用量记录）与 output_probes.json（模型上限探测）共用本规则。
func stateSibling(stateFile, name string) string {
	dir := filepath.Dir(stateFile)
	if dir == "" || dir == "." {
		return name
	}
	return filepath.Join(dir, name)
}

func main() {
	cfgPath := flag.String("config", "config.json", "配置文件路径（默认当前目录 config.json；不存在时自动生成推荐配置）")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		// errors.Is 才能看穿 Load 里 fmt.Errorf("%w") 的包装；os.IsNotExist 不行。
		if errors.Is(err, fs.ErrNotExist) {
			// 首次运行：目录下没有配置 → 自动落一份推荐配置（含随机 api_key）再加载。
			// 双击 exe / 裸跑 docker 即开，无需先手工复制样例。
			if key, werr := WriteDefault(*cfgPath); werr == nil {
				log.Printf("config %s 不存在，已生成推荐配置（api_key=%s，记录在该文件里，可自行修改）", *cfgPath, key)
				cfg, err = Load(*cfgPath)
			}
			if err != nil {
				// 生成失败（目录只读等）：退回纯默认 + env（旧行为兜底），不阻塞启动。
				log.Printf("config %s not found (auto-generate failed), using defaults+env: %v", *cfgPath, err)
				cfg, err = Load("")
			}
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)

	p := pool.New(cfg.StateFile)
	// 停机序：先 pool.Close()（最后一次 Flush → SaveState 已提交到 store），
	// 再 store.Close() 排空在途异步写（最后一笔 Redis 镜像必须写完才关连接）。
	defer func() {
		p.Close()
		_ = store.Close()
	}()
	p.SetStore(store)
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
	p.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）

	// 熔断器 + 在途上限（含 global 分档）+ 连败降权 + 三因子加权调优（从 config 注入，
	// 非正值回退默认）。
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(cfg.Pool.MaxInFlightGlobal) // global 域 WAF 风控分档（P1-1）
	p.SetDegrade(cfg.Pool.DegradeThreshold, cfg.DegradeCooldownDur, cfg.DegradeCooldownMaxD)
	p.SetSoftRateMax(cfg.SoftRateMaxDur)                 // 软冷却指数退避封顶（soft_rate_max，默认 2h）
	p.SetCostExploreInterval(cfg.CostExploreIntervalDur) // costTier 探索窗口（issue #136，默认 30m；0 关停）
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)

	// 会话粘性路由（可配关闭）。
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  p.AvailableUIDs,
			// realm 感知闭包：带前缀模型名按 realm 过滤可用账号（跨 realm 不泄漏）；
			// 裸名走 cn（现状零回归）。闭包内部 resolveModel 剥前缀，再按 realm 过滤。
			AvailableForModel: realmAwareAvailableForModel(p),
		})
		sessRouter.LoadFromStore() // 启动时从 Redis 恢复粘性（读操作仅此处）
		sessRouter.StartGC()
		defer sessRouter.StopGC()
	}
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	up := upstream.New()
	// 短 RPC 总时长上限（refresh/checkin/balance/FetchModels），语义不变。
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 聊天 SSE 首字节前（响应头）上限：cfg 已 normalize（缺省回落 timeout_seconds）。
	up.HeaderTimeout = time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second
	if tr, ok := up.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HeaderTimeout
	}
	// 聊天 SSE 流中空闲上限（S3 空闲监控读取）。
	up.IdleTimeout = time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second
	up.SanitizeFingerprints = cfg.Features.SanitizeBlacklistFingerprints
	// 出站 UA 与归属头（issue #42 + 上游同步）：
	// UserAgent 非空则完全覆盖；ClientVersion/CliVersion 缺省对齐官方形态；
	// ClientName 非空时 chat 路径注入 X-IDE-* 四头（用量归因对齐官方桌面端）。
	up.UserAgent = cfg.Upstream.UserAgent
	up.ClientVersion = cfg.Upstream.ClientVersion
	up.CliVersion = cfg.Upstream.CliVersion
	up.ClientName = cfg.Upstream.ClientName
	up.DeviceToken = cfg.Upstream.DeviceToken
	up.DeviceTokenFile = cfg.Upstream.DeviceTokenFile
	up.PassthroughIP = cfg.Upstream.PassthroughIP
	// global realm 路由（config global 段）：上游侧开关（第一道闸）+ base 覆盖；
	// auth 侧开关（auth.SetGlobalEnabled）是第二道闸，两者同 config global.enabled。
	up.GlobalEnabled = cfg.Global.Enabled
	up.ChatBaseGlobal = cfg.Global.ChatBase
	up.BillingBaseGlobal = cfg.Global.BillingBase
	auth.SetGlobalEnabled(cfg.Global.Enabled)
	// model.json 本地缓存接线（context_length/max_output_tokens 四级查找链第 3 级）：
	// 数据目录与 state.json 同风格（Docker volume 持久化路径）。首次缺失/损坏自动
	// 回落仓库内嵌种子；models.dev 按需拉取成功后原子写回。
	upstream.SetModelCatalogPath(stateSibling(cfg.StateFile, "model.json"))

	sch := scheduler.New(scheduler.Config{
		Pool:           p,
		Upstream:       up,
		CheckinHours:   cfg.Schedule.CheckinHours,
		TravelHours:    cfg.Schedule.TravelHours,
		ActivityHours:  cfg.Schedule.ActivityHours,
		KeepaliveHours: cfg.Schedule.KeepaliveHours,
		BlackcatHours:  cfg.Schedule.BlackcatHours,
		// 快过期积分优先消耗：签到/余额刷新按此窗口分桶（issue:积分过期）。
		ExpiringSoonWindow: cfg.ExpiringSoonDur,
		CheckinDisabled:    !cfg.Schedule.CheckinEnabled,
		TravelDisabled:     !cfg.Schedule.TravelEnabled,
		ActivityDisabled:   !cfg.Schedule.ActivityEnabled,
		KeepaliveDisabled:  !cfg.Schedule.KeepaliveEnabled,
		BlackcatDisabled:   !cfg.Schedule.BlackcatEnabled,
	})
	switch {
	case !cfg.Schedule.CheckinEnabled:
		log.Printf("签到已禁用（schedule.checkin_enabled=false）")
	default:
		log.Printf("签到已启用：%v 点（签到 + 余额查询解冻）", cfg.Schedule.CheckinHours)
	}
	switch {
	case !cfg.Schedule.TravelEnabled:
		log.Printf("猫猫旅行已禁用（schedule.travel_enabled=false）")
	default:
		log.Printf("猫猫旅行已启用：%v 点（独立排程：领养 / 派出 / 领奖）", cfg.Schedule.TravelHours)
	}
	switch {
	case !cfg.Schedule.ActivityEnabled:
		log.Printf("活跃上报已禁用（schedule.activity_enabled=false）")
	default:
		log.Printf("活跃上报已启用：%v 点（每日 1 次，点亮连登 + 解锁 first_buddy）", cfg.Schedule.ActivityHours)
	}
	if !cfg.Schedule.KeepaliveEnabled {
		log.Printf("token 保活已禁用（schedule.keepalive_enabled=false）")
	} else {
		log.Printf("token 保活已启用：%v 点", cfg.Schedule.KeepaliveHours)
	}
	switch {
	case !cfg.Schedule.BlackcatEnabled:
		log.Printf("夜猫子已禁用（schedule.blackcat_enabled=false）")
	default:
		log.Printf("夜猫子已启用：%v 点（23:00–08:00 窗口 glm-5.2 对话补足）", cfg.Schedule.BlackcatHours)
	}
	switch {
	case !cfg.Schedule.BalanceRefreshEnabled:
		log.Printf("余额后台刷新已禁用（schedule.balance_refresh_enabled=false）")
	case cfg.BalanceRefreshInterval > 0:
		log.Printf("余额后台刷新：每 %s（签到时点照常额外刷新）", cfg.BalanceRefreshInterval)
	}

	// 管理面板日志镜像：标准 log（stderr）与 chat 表格日志（stdout）双路复制进
	// 面板环形缓冲，供 /panel/api/logs 读取；控制台输出行为完全不变。
	// live 承载可热改字段（api_key/soft_rate/脱敏开关），面板保存配置时在线替换。
	live := livecfg.New(livecfg.Snapshot{
		APIKey:               cfg.APIKey,
		SoftCooldown:         cfg.SoftRateDur,
		SanitizeFingerprints: cfg.Features.SanitizeBlacklistFingerprints,
	})
	// 用量记录器：与 state 文件同目录，随 state_file 配置一起搬移。
	// datapath 由 state 文件路径推出，避免再加一个配置项。
	usagePath := usagePathFor(cfg.StateFile)
	rec := usage.New(usagePath)
	rec.Start()
	defer rec.Stop()
	log.Printf("[usage] 逐请求用量记录已启用: %s (%s)", usagePath, rec.Describe())

	pn := panel.New(panel.Config{
		Pool:        p,
		Usage:       rec,
		Upstream:    up,
		Scheduler:   sch,
		AuthDir:     cfg.AuthDir,
		APIKey:      cfg.APIKey,
		RedisMode:   redisMode,
		StickyCount: sessCount,
		Version:     appVersion,
		Live:        live,
		// 模型上限探测数据（scripts/probe_max_tokens.py --panel-out 写入）：
		// 与 state 文件同目录，缺省 data/output_probes.json。
		ProbeFile:  stateSibling(cfg.StateFile, "output_probes.json"),
		ConfigPath: *cfgPath,
		LoadConfig: func() (any, error) {
			return Load(*cfgPath)
		},
		SaveConfig: func(raw []byte) ([]string, error) {
			return saveConfig(raw, *cfgPath, live, p, up, sch)
		},
	})
	log.SetOutput(io.MultiWriter(os.Stderr, pn.Logs()))
	server.SetChatLogOutput(io.MultiWriter(os.Stdout, pn.Logs()))

	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     up,
		APIKey:       cfg.APIKey,
		Session:      sessRouter,
		StickyCount:  sessCount,
		RedisMode:    redisMode,
		SoftCooldown: cfg.SoftRateDur,
		Panel:        pn,
		Live:         live,
		Usage:        rec,
		PromptMode:   cfg.Prompt.Mode,
		PromptText:   cfg.PromptText,
		// handler 侧第三道闸（global realm）：false（显式逃生门）时不列 global: 模型名。
		GlobalEnabled: cfg.Global.Enabled,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)
	sch.StartBalanceRefresh(ctx, cfg.BalanceRefreshInterval)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		// ReadTimeout 覆盖整个请求读取（含 body）：防慢速 body 拖死连接。
		// 请求体已无网关侧上限（max_body_mb 移除），60s 按常规带宽的数十 MB
		// 上传余量取值；超大 body 慢速上传若超时，由客户端重试。
		ReadTimeout: 60 * time.Second,
		// IdleTimeout keep-alive 空闲连接回收：配合 chat 出站 ctx 传播防连接泄漏堆积。
		// 注意：SSE 流式响应期间连接非空闲，不受此项掐断；不设全局 WriteTimeout
		// （长流式生成合法时长可达数分钟，全局 WriteTimeout 会误杀在途 SSE）。
		IdleTimeout: 120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		p.Flush() // 信号触发：先落盘再做优雅停机
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("workbuddy2api listening on %s (api_key=%v)，管理面板 http://127.0.0.1%s/panel/", cfg.Listen, cfg.APIKey != "", panelListenPath(cfg.Listen))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}

// panelListenPath 从 listen 地址提取 ":port" 形式，用于启动日志拼面板 URL
// （":7863" 或 "0.0.0.0:7863" → ":7863"；异常输入原样返回）。
func panelListenPath(listen string) string {
	for i := len(listen) - 1; i >= 0; i-- {
		if listen[i] == ':' {
			return listen[i:]
		}
	}
	return listen
}

// saveConfig 面板保存配置：校验 → 落盘 → 热应用 → 返回需重启的字段列表。
//
// 热生效范围（设计取舍）：
//   - api_key / cooldown.soft_rate / features.sanitize_blacklist_fingerprints → livecfg 快照
//   - pool.* → pool.SetBreaker/SetMaxInFlight/SetSoftRateMax/SetWeights/SetCostExploreInterval
//   - schedule.* → scheduler.Reconfigure/SetBalanceInterval
//
// 需重启（涉及监听地址、HTTP client 超时、auth_dir 等装配期依赖）：
//   - listen / auth_dir / state_file / upstream.* / upstash.* / session_sticky.*（TTL 类）
//
// 落盘用"先写 tmp 再 rename"原子替换，且优先保留磁盘上的原始 JSON 结构（只改
// 面板表单覆盖到的键），避免把用户手写的注释性字段/未知键洗掉——这里直接整体
// 序列化校验后的配置，未知键在 json.Unmarshal 时已丢失，故先合并原始 map。
func saveConfig(raw []byte, path string, live *livecfg.Holder, p *pool.Pool, up *upstream.Client, sch *scheduler.Scheduler) ([]string, error) {
	// 1) 解析原始 JSON 为 map（保留用户手写的未知键），再叠加面板提交的键。
	oldRaw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read current config: %w", err)
	}
	var cur, incoming map[string]any
	if err := json.Unmarshal(oldRaw, &cur); err != nil {
		cur = map[string]any{}
	}
	if err := json.Unmarshal(raw, &incoming); err != nil {
		return nil, fmt.Errorf("parse submitted config: %w", err)
	}
	merged := mergeConfigMaps(cur, incoming)

	// 2) 校验（与启动同一套 Default+normalize），失败直接返回、不落盘。
	newCfg, err := ParseConfig(mergedJSON(merged))
	if err != nil {
		return nil, err
	}

	// 3) 落盘（原子替换）。
	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return nil, fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		// A single-file Docker bind mount cannot be renamed over its mount
		// target (Linux returns EBUSY / "device or resource busy"). Keep the
		// atomic path for regular files, but update the mounted file in place
		// for this specific deployment shape.
		if !errors.Is(err, syscall.EBUSY) {
			return nil, fmt.Errorf("replace config: %w", err)
		}
		f, openErr := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
		if openErr != nil {
			_ = os.Remove(tmp)
			return nil, fmt.Errorf("replace config (bind mount fallback): %w", openErr)
		}
		_, writeErr := f.Write(out)
		if writeErr == nil {
			writeErr = f.Sync()
		}
		closeErr := f.Close()
		// 写失败时保留 tmp（挂载文件已被 O_TRUNC 破坏，tmp 里是完整新内容，
		// 可手工恢复）；写成功才清理。
		if writeErr != nil {
			return nil, fmt.Errorf("replace config (bind mount fallback, 完整新内容保留在 %s): %w", tmp, writeErr)
		}
		_ = os.Remove(tmp)
		if closeErr != nil {
			return nil, fmt.Errorf("replace config (bind mount fallback): %w", closeErr)
		}
	}

	// 4) 热应用：能立即生效的字段全部应用，并列出仍需重启的字段。
	live.Store(livecfg.Snapshot{
		APIKey:               newCfg.APIKey,
		SoftCooldown:         newCfg.SoftRateDur,
		SanitizeFingerprints: newCfg.Features.SanitizeBlacklistFingerprints,
	})
	up.SanitizeFingerprints = newCfg.Features.SanitizeBlacklistFingerprints
	p.SetBreaker(newCfg.Pool.BreakerThreshold, newCfg.BreakerCooldownDur, newCfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(newCfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(newCfg.Pool.MaxInFlightGlobal)
	p.SetDegrade(newCfg.Pool.DegradeThreshold, newCfg.DegradeCooldownDur, newCfg.DegradeCooldownMaxD)
	p.SetSoftRateMax(newCfg.SoftRateMaxDur)
	p.SetCostExploreInterval(newCfg.CostExploreIntervalDur) // costTier 探索窗口热生效（0 关停）
	p.SetWeights(newCfg.Pool.IdleWeightPerHour, newCfg.Pool.IdleWeightMax)
	sch.Reconfigure(
		newCfg.Schedule.CheckinHours, newCfg.Schedule.TravelHours,
		newCfg.Schedule.ActivityHours, newCfg.Schedule.KeepaliveHours, newCfg.Schedule.BlackcatHours,
		!newCfg.Schedule.CheckinEnabled, !newCfg.Schedule.TravelEnabled,
		!newCfg.Schedule.ActivityEnabled, !newCfg.Schedule.KeepaliveEnabled, !newCfg.Schedule.BlackcatEnabled)
	sch.SetBalanceInterval(newCfg.BalanceRefreshInterval)

	return restartRequiredFields(newCfg), nil
}

// restartRequiredFields 返回本次改动中无法热生效、需要重启进程的字段名。
// 恒返回完整清单中的"与当前进程装配期依赖相关"的项——面板据此提示用户。
func restartRequiredFields(c *Config) []string {
	var out []string
	// 这些字段在进程内被监听地址/HTTP client/目录句柄等装配期对象捕获。
	if c.Listen != "" {
		out = append(out, "listen")
	}
	if c.AuthDir != "" {
		out = append(out, "auth_dir")
	}
	if c.StateFile != "" {
		out = append(out, "state_file")
	}
	out = append(out, "upstream.timeout_seconds", "upstream.header_timeout_seconds", "upstream.idle_timeout_seconds")
	if c.Upstash.URL != "" || c.Upstash.Token != "" {
		out = append(out, "upstash")
	}
	out = append(out, "session_sticky.ttl", "session_sticky.gc_interval")
	return out
}

// mergeConfigMaps 把 incoming 深合并进 cur（原地），返回 cur。
// 对嵌套对象逐键覆盖而不是整体替换：面板表单只提交它管理的键，
// 未提交的兄弟键（含用户手写的未知键）保持原样。
func mergeConfigMaps(cur, incoming map[string]any) map[string]any {
	for k, v := range incoming {
		if inMap, ok := v.(map[string]any); ok {
			if curMap, ok := cur[k].(map[string]any); ok {
				cur[k] = mergeConfigMaps(curMap, inMap)
				continue
			}
		}
		cur[k] = v
	}
	return cur
}

// mergedJSON 把合并后的 map 序列化回 JSON（供 ParseConfig 校验）。
func mergedJSON(m map[string]any) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}
