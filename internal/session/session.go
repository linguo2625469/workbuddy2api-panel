// Package session 会话粘性路由：同一会话（conversationId / metadata 键）尽量绑定同一账号。
//
// 设计参考 antigravityProxyGo internal/session（fast-path RLock / 双段分配 / TTL / 持久化），
// 但改为纯内存 + redisstore 异步镜像：
//   - 命中走 RLock 快查（绝大多数请求已绑定）；
//   - 未命中/失效走写锁 re-check 后分配，避免同 key 并发重复分配（TOCTOU 防护）；
//   - 分配优先"空闲账号"（未绑定任何会话的可用号）哈希，其次全池哈希（双段策略）；
//   - LastActive 滚动续期，TTL 过期由后台 GC 或快路径惰性过期清理；
//   - 每次绑定变更 fire-and-forget 镜像到 redisstore（防重启丢粘性）。
package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/redisstore"
)

// entry 单条会话绑定。
type entry struct {
	uid        string
	lastActive time.Time
}

// Config 路由依赖；Available 返回"可用账号"（healthy 且未占满在途）的有序 uid 列表，
// 由 pool.AvailableUIDs 提供。Store 可为 redisstore.Noop（纯内存）。
type Config struct {
	TTL        time.Duration
	GCInterval time.Duration
	Store      redisstore.Store
	Available  func() []string
	// AvailableForModel 按请求模型返回"在该模型上可用"的账号（healthy 且未占满在途，
	// 且未被该模型限流/限额）。nil 时回落 Available（无模型维度，行为与引入前一致）。
	//
	// 为什么粘性需要模型维度：绑定只记 uid，而同一个会话可能换模型。账号被 6004
	// 模型级限额后对**其他模型**仍可用（issue #31 豁免），此时若只按账号级可用性
	// 校验，会话会被钉在这个号上反复失败——正是"限额后换不动号"的观感来源。
	AvailableForModel func(model string) []string
}

// Router 会话粘性路由器。
type Router struct {
	mu      sync.RWMutex
	entries map[string]entry
	cfg     Config
	stop    chan struct{}
}

// New 构建路由器。若 cfg.Store 为 nil 则用 Noop（纯内存）；cfg.Available 为 nil 视为空池。
// TTL/GCInterval 非正取默认（30m / 5m）——main 从 config 解析后传入，这里兜底。
func New(cfg Config) *Router {
	if cfg.Store == nil {
		cfg.Store = redisstore.Noop{}
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 30 * time.Minute
	}
	if cfg.GCInterval <= 0 {
		cfg.GCInterval = 5 * time.Minute
	}
	return &Router{entries: map[string]entry{}, cfg: cfg}
}

// StartGC 启动后台 GC goroutine（幂等）。进程退出时调 StopGC。
//
// stop channel 必须在启 goroutine 前捕获到**局部变量**：goroutine 在 select 里
// 每轮重新求值 r.stop 是无锁读，而 StopGC 持写锁把它置 nil——既是数据竞争
// （-race 可复现），又会在读到 nil 后让该 case 永久阻塞（nil channel 永不就绪），
// 于是关停彻底失效：goroutine 再也不会退出，ticker 无限触发 gcOnce（goroutine
// 泄漏 + 关停后仍持续 GC）。捕获局部变量后，close(stop) 与 select 观测的是同一个
// channel，StopGC 一定能让 goroutine 退出。
func (r *Router) StartGC() {
	r.mu.Lock()
	if r.stop != nil {
		r.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	r.stop = stop
	r.mu.Unlock()

	go func() {
		t := time.NewTicker(r.cfg.GCInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				r.gcOnce(time.Now())
			}
		}
	}()
}

// StopGC 停止后台 GC（幂等）。
func (r *Router) StopGC() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stop != nil {
		close(r.stop)
		r.stop = nil
	}
}

// LoadFromStore 启动时从 redisstore 恢复绑定（内存覆盖本地，读操作仅此处发生）。
// 已有本地绑定被保留——Redis 仅为恢复备份，本地一旦建立即为权威。
func (r *Router) LoadFromStore() {
	binds := r.cfg.Store.LoadBinds()
	if len(binds) == 0 {
		return
	}
	now := time.Now()
	r.mu.Lock()
	loaded := 0
	for key, uid := range binds {
		if _, exists := r.entries[key]; exists {
			continue
		}
		r.entries[key] = entry{uid: uid, lastActive: now}
		loaded++
	}
	r.mu.Unlock()
	if loaded > 0 {
		log.Printf("[session] 从 Redis 恢复 %d 条粘性会话绑定", loaded)
	}
}

// Resolve 返回会话 key 应绑定的账号 uid，ok=false 表示当前无可用账号。
// 无模型维度（等价于 ResolveForModel(key, "")），保留给不关心模型的调用方。
func (r *Router) Resolve(key string) (string, bool) {
	return r.ResolveForModel(key, "")
}

// LookupForModel 只复用已有有效绑定，不通过哈希为新会话分配账号。
// 有序账号池策略用它保留粘性，未绑定会话由 Pool 选号并在成功后 Bind。
func (r *Router) LookupForModel(key, model string) (string, bool) {
	now := time.Now()
	available := r.availableSet(model)
	r.mu.Lock()
	defer r.mu.Unlock()
	e, found := r.entries[key]
	if !found || expired(e, now, r.cfg.TTL) || !available[e.uid] {
		return "", false
	}
	r.entries[key] = entry{uid: e.uid, lastActive: now}
	r.cfg.Store.SetBind(key, e.uid, r.cfg.TTL)
	return e.uid, true
}

// ResolveForModel 返回会话 key 在该模型上应绑定的账号 uid。
// 命中且账号在该模型可用 → 滚动 lastActive 并直接返回；否则（绑定号已冷却/占满/
// 被该模型限流）走重新分配。
//
// 为什么必须带模型：绑定只记 uid，同一个会话可能换模型；账号被 6004 模型级限额后
// 对其他模型仍可用（见 pool.healthyForModel 的 softRateModel 豁免）。若只按账号级
// 可用性校验，会话会被钉在一个"对当前模型不可用"的号上反复失败。
func (r *Router) ResolveForModel(key, model string) (string, bool) {
	now := time.Now()
	available := r.availableSet(model)

	// ── Fast path: RLock 快查 ──────────────────────────────
	r.mu.RLock()
	e, found := r.entries[key]
	r.mu.RUnlock()
	if found && !expired(e, now, r.cfg.TTL) {
		if available[e.uid] {
			r.touch(key, e.uid, now)
			return e.uid, true
		}
		// 绑定号已冷却/占满 → 失效，落入慢路径重分配。
	}

	// ── Slow path: 写锁 re-check 后分配 ────────────────────
	r.mu.Lock()
	defer r.mu.Unlock()

	// re-check：并发同 key 可能已被其他 goroutine 分配好。
	if e2, found2 := r.entries[key]; found2 && !expired(e2, now, r.cfg.TTL) {
		if available[e2.uid] {
			r.entries[key] = entry{uid: e2.uid, lastActive: now}
			return e2.uid, true
		}
		delete(r.entries, key) // 失效：清掉再分配
	}

	uids := r.availableSlice(model)
	if len(uids) == 0 {
		return "", false
	}

	// 双段策略：优先"空闲账号"（未被任何会话绑定的可用号），其次全池。
	bound := map[string]bool{}
	for _, v := range r.entries {
		bound[v.uid] = true
	}
	var idle []string
	for _, u := range uids {
		if !bound[u] {
			idle = append(idle, u)
		}
	}
	pool2 := idle
	if len(pool2) == 0 {
		pool2 = uids
	}
	uid := pool2[hashIndex(key, len(pool2))]

	prev, existed := r.entries[key]
	r.entries[key] = entry{uid: uid, lastActive: now}
	if existed && prev.uid != uid {
		r.cfg.Store.DelBind(key)
	}
	r.cfg.Store.SetBind(key, uid, r.cfg.TTL)
	return uid, true
}

// touch 滚动 lastActive 并异步镜像（只在快路径命中时写最后一次）。
func (r *Router) touch(key, uid string, now time.Time) {
	r.mu.Lock()
	r.entries[key] = entry{uid: uid, lastActive: now}
	r.mu.Unlock()
	r.cfg.Store.SetBind(key, uid, r.cfg.TTL)
}

// Bind 显式把会话 key 绑定到 uid（幂等覆盖旧值），并异步镜像到 redisstore。
// 供"粘性跟随最终成功号"用：请求成功返回前，把会话重绑到实际成功的账号，让多轮对话下一跳稳定
// 收敛到"对该会话持续成功的号"（对齐 antigravity 语义）。空 key 直接返回（无会话则不绑）。
func (r *Router) Bind(key, uid string) {
	if key == "" || uid == "" {
		return
	}
	now := time.Now()
	r.mu.Lock()
	r.entries[key] = entry{uid: uid, lastActive: now}
	r.mu.Unlock()
	r.cfg.Store.SetBind(key, uid, r.cfg.TTL)
}

// Unbind 解除会话绑定（请求失败时调用，让该会话下次重新分配）。返回是否存在。
func (r *Router) Unbind(key string) bool {
	r.mu.Lock()
	_, found := r.entries[key]
	if found {
		delete(r.entries, key)
	}
	r.mu.Unlock()
	if found {
		r.cfg.Store.DelBind(key)
	}
	return found
}

// Count 返回当前绑定数（供 /status 观测）。
func (r *Router) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

// gcOnce 清理 TTL 过期的绑定，并镜像删除。
func (r *Router) gcOnce(now time.Time) int {
	r.mu.Lock()
	var expiredKeys []string
	for key, e := range r.entries {
		if now.Sub(e.lastActive) > r.cfg.TTL {
			expiredKeys = append(expiredKeys, key)
		}
	}
	for _, key := range expiredKeys {
		delete(r.entries, key)
	}
	r.mu.Unlock()
	for _, key := range expiredKeys {
		r.cfg.Store.DelBind(key)
	}
	return len(expiredKeys)
}

// availableSet 把 AvailableForModel(model) 的有序列表转集合（快路径命中校验用）。
func (r *Router) availableSet(model string) map[string]bool {
	uids := r.availableSlice(model)
	set := make(map[string]bool, len(uids))
	for _, u := range uids {
		set[u] = true
	}
	return set
}

// availableSlice 安全调用 AvailableForModel；未注入时回落 Available（nil 视空池）。
func (r *Router) availableSlice(model string) []string {
	if r.cfg.AvailableForModel != nil {
		return r.cfg.AvailableForModel(model)
	}
	if r.cfg.Available == nil {
		return nil
	}
	return r.cfg.Available()
}

func expired(e entry, now time.Time, ttl time.Duration) bool {
	return now.Sub(e.lastActive) > ttl
}

// hashIndex FNV-1a 哈希取模（antigravity 双段分配的稳定散列）。
func hashIndex(key string, n int) int {
	var h uint32 = 2166136261
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return int(h % uint32(n))
}

// ExtractKey 从请求体提取会话键；按下列顺序依次尝试，找不到时回退到
// 内容派生的稳定键（见 deriveKey），仍为空则返回空串（绝不失败）。
//  1. metadata.conversation_id
//  2. metadata.conversationId
//  3. conversation_id
//  4. conversationId
//  5. prompt_cache_key（OpenAI 前缀缓存键，见下）
//  6. 派生键：system 提示词 + 首条用户消息的哈希（客户端不发会话 id 时的回退，
//     带 user_id 的请求不派生——见下方契约说明）
//
// 前 5 项均为 conversation 维度（对话级）。metadata.user_id 不再作为粘性键
// （P1-anti-monopoly 剔除）：user 维度粒度过粗——一个 user 的全部并行对话
// 会钉同一账号（粘性范围远大于上游 prompt cache 的对话级边界），且曾抢占顶层
// conversation_id 的优先级。剔除后发 user_id 的客户端回落加权轮换（与无标识
// 客户端同路径），旧 user_id 绑定靠 TTL 自然过期，键消失不产生脏绑定。
//
// issue #35：客户端实际发 camelCase 的 conversationId，此前只识别 snake_case，
// 导致粘性路由不命中、同对话轮转不同账号、上游上下文缓存 miss。现两种命名均识别，
// snake_case 优先级高于 camelCase（同值不同名命中同一对话时返回相同值，天然不混用）。
//
// 第 5 项 prompt_cache_key：pi-ai 驱动的客户端（dsh 等）把会话 ID 放在这个 OpenAI
// 前缀缓存字段里（而非 conversation_id），网关在 upstream 侧本就认它（见
// InjectPromptCacheKey 优先级 1：客户端自带则原值保留）。纳入识别后，这类客户端
// 无需改配置即可命中粘性。置于显式会话键之后、派生键之前，绝不抢占 conversation
// 维度的优先级。
//
// 派生键的 user_id 抑制（P1-anti-monopoly 契约在 fallback 路径的延伸）：body 携带
// metadata.user_id 或顶层 user_id 时**不派生**。ExtractKey 有意剔除 user_id 作粘性键
// （user 维度粒度过粗——一个 user 的全部并行对话会被钉到同一账号），若派生路径不设
// 此闸，只发 user_id 的请求会借内容哈希重新获得粘性，使该契约在 fallback 路径失效
// （上游 #169 同款回归修正）。这类客户端回落加权轮换。
// chatMessage 是单次解析里一条消息的最小投影：只取 role / content，content 保留
// 原始字节（json 侧会拷贝，不别名 body），供会话键与轮键各自按需再解。
type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// chatRequest 是 ExtractKey / TurnKey 共同需要的请求体投影。标量字段一律用
// RawMessage 承接（经 rawString 取串）：原 map[string]any 方案对"键存在但值非
// 字符串/非对象"天然容忍（按零值跳过），强类型 struct 会在整个 Unmarshal 上报错
// 从而误伤——RawMessage 逐字段取值把这份容忍原样保留。
type chatRequest struct {
	Metadata        json.RawMessage   `json:"metadata"`
	ConversationID  json.RawMessage   `json:"conversation_id"`
	ConversationID2 json.RawMessage   `json:"conversationId"`
	PromptCacheKey  json.RawMessage   `json:"prompt_cache_key"`
	UserID          json.RawMessage   `json:"user_id"`
	Messages        []json.RawMessage `json:"messages"`
}

// RequestKeys 一次 unmarshal body 后同时派生会话键（ExtractKey）与轮级键
// （TurnKey），供 handler 在 prompt.Rewrite 之前只扫一遍请求体（长历史消息是
// 请求体里最大的部分，此前这两个函数各解一遍 map / struct，等于两趟全量解析）。
// 返回的两个值与分别调用 ExtractKey / TurnKey 的结果逐字节一致（同一份投影、
// 同一套优先级与签名口径，见两者的文档契约）。
func RequestKeys(body []byte) (sessionKey, turnKey string) {
	if len(body) == 0 {
		return "", ""
	}
	var req chatRequest
	if json.Unmarshal(body, &req) != nil {
		return "", ""
	}

	// ---- 会话键：优先级与 ExtractKey 完全一致 ----
	var meta map[string]json.RawMessage
	if len(req.Metadata) > 0 {
		_ = json.Unmarshal(req.Metadata, &meta) // 非对象 → nil，按无 metadata 处理
	}
	if v := rawString(meta["conversation_id"]); v != "" {
		sessionKey = v
	} else if v := rawString(meta["conversationId"]); v != "" {
		sessionKey = v
	} else if v := rawString(req.ConversationID); v != "" {
		sessionKey = v
	} else if v := rawString(req.ConversationID2); v != "" {
		sessionKey = v
	} else if v := rawString(req.PromptCacheKey); v != "" {
		sessionKey = v
	} else {
		// 派生回退闸：带 user 维度标识的请求不派生（契约见 ExtractKey 文档）。
		if rawString(meta["user_id"]) != "" || rawString(req.UserID) != "" {
			sessionKey = ""
		} else {
			sessionKey = deriveKey(req.Messages)
		}
	}

	// ---- 轮级键：最后一条 user 消息的「序号 + 内容签名」（口径同 TurnKey） ----
	for i := len(req.Messages) - 1; i >= 0; i-- {
		var m chatMessage
		if json.Unmarshal(req.Messages[i], &m) != nil || m.Role != "user" {
			continue
		}
		sig := contentSignature(m.Content)
		if sig == "" {
			// 最后一条 user 消息无可签名内容：本轮不建立聚合键，也不往前找
			//（位置恒定，往前找会让键随 step 漂移——契约见 TurnKey 文档）。
			return sessionKey, ""
		}
		return sessionKey, fmt.Sprintf("u%d:%s", i, sig)
	}
	return sessionKey, ""
}

// ExtractKey 从请求体提取会话键（只解一遍）。语义与优先级见 RequestKeys 上方
// ExtractKey 的历史契约文档；本函数保留为薄包装供单测/潜在外部调用复用。
func ExtractKey(body []byte) string {
	k, _ := RequestKeys(body)
	return k
}

// derivedKeyPrefix 派生键前缀，与显式会话 id 的命名空间隔离：
// 即便客户端恰好传了形如 "d-<hex>" 的显式 id 也不至于与派生键混淆（显式 id 优先返回）。
const derivedKeyPrefix = "d-"

// deriveKey 从消息内容派生稳定会话键：SHA-256(system 文本 + 首条 user 文本) 前 16 字节。
//
// 为什么用「system + 首条 user」而不是全部消息：
//   - 多轮对话里历史消息每轮追加，全量哈希会每轮变化 → 粘性完全失效；
//   - system 与首条 user 在一次对话中恒定，足以区分不同对话；
//   - 同一会话多轮请求 → 同一键 → 稳定粘住同一账号（上游 prompt 缓存命中）。
//
// 入参为已单条 RawMessage 化的 messages（跳过解不开的非对象元素，与旧 map
// 方案的类型断言跳过同语义）。取不到用户文本（纯图片等）时返回空串：不粘性，
// 退回普通轮换（安全降级）。
func deriveKey(rawMsgs []json.RawMessage) string {
	if len(rawMsgs) == 0 {
		return ""
	}
	systemText, firstUserText := "", ""
	for _, raw := range rawMsgs {
		var m chatMessage
		if json.Unmarshal(raw, &m) != nil {
			continue // 非对象元素：与旧 map[string]any 的类型断言跳过一致
		}
		switch m.Role {
		case "system", "developer":
			if systemText == "" {
				systemText = messageText(m.Content)
			}
		case "user":
			if firstUserText == "" {
				// 首条 user 用会话级稳定签名（[type] 占位、不入图片摘要——见
				// userContentSignature 文档），纯图片轮也能派生出非空键。
				firstUserText = userContentSignature(m.Content)
			}
		}
		if firstUserText != "" && systemText != "" {
			break // 都已拿到：停止遍历长历史
		}
	}
	if firstUserText == "" {
		return "" // 无用户消息：无从归属会话
	}
	sum := sha256.Sum256([]byte(systemText + "\x00" + firstUserText))
	return derivedKeyPrefix + hex.EncodeToString(sum[:16])
}

// userContentSignature 取消息 content 的**会话级稳定**签名，供 deriveKey（粘性键）
// 专用。与 TurnKey 的 contentSignature（全文摘）不同：文本 part 照常拼接；非文本
// part 只入 "[type]" 占位**不入内容摘要**——同一逻辑图片的 URL 逐轮变化（签名
// URL/重编码 base64）是真实场景，内容摘要入键会让派生键随轮漂移、粘性名存实亡
// （fork 既有契约：image url changes must not break derived key stability）。占位
// 仍能区分"有无图片/图片数量"，纯图片首条消息（无 text part）由此可派生非空键
// （首图会话粘性盲区修复，对齐上游 G1 的目标语义而保留 fork 的稳定性口径）。
// 形态异常按无内容处理（不伪造）。
func userContentSignature(raw json.RawMessage) string {
	st := strings.TrimSpace(string(raw))
	if st == "" || st == "null" {
		return ""
	}
	if st[0] == '"' {
		var str string
		if json.Unmarshal(raw, &str) != nil {
			return ""
		}
		return str
	}
	if st[0] != '[' {
		return ""
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	hasNonText := false
	for _, pr := range parts {
		var p struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(pr, &p) != nil {
			return ""
		}
		if p.Type == "" || p.Type == "text" {
			b.WriteString(p.Text)
			continue
		}
		hasNonText = true
		b.WriteString("\n[" + p.Type + "]\n")
	}
	out := b.String()
	if !hasNonText {
		return out
	}
	return strings.TrimSpace(out)
}

// messageText 提取消息 content 的文本表示。
// 兼容：字符串 / [{type:"text",text:"..."}] 数组（OpenAI 多模态）；其他类型取空。
func messageText(raw json.RawMessage) string {
	st := strings.TrimSpace(string(raw))
	if st == "" || st == "null" {
		return ""
	}
	if st[0] == '"' {
		var str string
		if json.Unmarshal(raw, &str) != nil {
			return ""
		}
		return str
	}
	if st[0] != '[' {
		return ""
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var sb strings.Builder
	for _, pr := range parts {
		var p struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(pr, &p) == nil {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// rawString 从 JSON 标量原始字节取字符串值；字段缺失（nil）、null 或非字符串
// 形态一律返回 ""（等价旧 map[string]any + strOrEmpty 的容忍语义）。
func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}
