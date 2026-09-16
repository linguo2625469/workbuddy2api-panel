// global 模型目录探测：产出模型名及其窗口 / 能力元数据，但**不产倍率**
// （PLAN §3.D2「模型名目录 ≠ 倍率表」）。
//
// credits 数值一律不进入本包实现——探测端点返回的倍率字段在解析阶段（parseGlobalModelInfos）
// 即被丢弃，元数据只喂 /v1/models 的 global: 前缀输出，不注入 costTier、不参与选号。
//
// 2026-09-16 修复：本包原先只产模型名（[]string），导致 handler 的 global 分支拿不到
// 窗口大小、只能输出裸名单，客户端回退到自身小默认值后**提前触发上下文压缩**。
// 现改为产出 []ModelInfo（与 CN 侧同构，上游两端点返回的 JSON 形状一致）。
package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// GlobalModelNames 国际版（global realm）模型名静态名单兜底（PLAN §7.2 附录 21 名）。
// 只含模型名、不含倍率与元数据。探测失败 / 无 global 账号时以此名单兜底（元数据留空，
// 窗口由 handler 侧兜底 131072）；探测成功时以其为基底，追加探测独有的模型名（去重）。
var GlobalModelNames = []string{
	"default-model",
	"fast-model",
	"balanced-model",
	"primary-model",
	"hy4-preview",
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"deep-model",
	"deepseek-v4.1-flash",
	"gpt-6-astra",
	"hy4-preview-f",
	"hy3",
	"glm-5.2",
	"gpt-5.6-luna",
	"gpt-5.5",
	"gpt-5.4",
	"gpt-5.3-codex",
	"gemini-3.5-flash",
	"glm-5.3",
	"kimi-k3",
	"kimi-k2.6",
}

// fetchGlobalModelsCache 探测结果缓存（语义参照 CN 侧 handler.dynamicModelsCache：1h TTL +
// 5min 失败负缓存）。按 Client 实例持有（effortsMu 同模式），测试新建 Client 即隔离。
// Mutex 内嵌，与 modelList 无并发读路径竞争（唯一读写点本文件内）。
type fetchGlobalModelsCache struct {
	sync.Mutex
	models   []ModelInfo // 成功缓存：探测 ∪ 静态名单（已去重）；nil = 未探测
	fetched  time.Time
	lastFail time.Time
}

// globalModelsTTL / globalModelsFailCooldown 探测缓存时长：成功 1h，失败 5min 负缓存。
const (
	globalModelsTTL          = time.Hour
	globalModelsFailCooldown = 5 * time.Minute
)

// globalModelsProbePaths global 模型目录端点候选序列（按 realm 切 base，路径"家族"）：
// /v2 家族优先（PR #20 实测 /v2/enterprises/personal/models 200 含完整模型表），
// /console 作 fallback（同域旧路径，或 500）。参考 PLAN v1 §2.2 分歧③ 与
// rockswang/wild-work PR #20 实测结论：console 路径在 global 上非 200 → 先 /v2。
var globalModelsProbePaths = []string{
	"/v2/enterprises/personal/models",
	"/console/enterprises/personal/models",
}

// FetchGlobalModels 探测 global 账号的模型名目录并返回**模型名列表**（无元数据）。
//
// 兼容入口：等价于 FetchGlobalModelInfos 后取 ID。新代码请直接用
// FetchGlobalModelInfos（需要窗口 / 能力元数据时）。
func (c *Client) FetchGlobalModels(a *auth.Auth) []string {
	infos := c.FetchGlobalModelInfos(a)
	out := make([]string, 0, len(infos))
	for _, mi := range infos {
		if mi.ID != "" {
			out = append(out, mi.ID)
		}
	}
	return out
}

// FetchGlobalModelInfos 探测 global 账号的模型目录并返回**带窗口 / 能力元数据**的条目列表。
//
// 成功：探测结果 ∪ GlobalModelNames（去重，静态 21 为基底，探测独有追加），缓存 1h；
// 静态独有条目（上游未返回，如 deepseek-v4.1-flash）元数据为零值，窗口由调用方兜底。
// 失败（家族端点全非 2xx / 解析失败 / 空列表）：记 5min 负缓存，回落 GlobalModelNames（无元数据）。
// 缓存/负缓存命中：直接返回，零上游调用。
//
// 调用方负责：① 仅在有 global 账号时调用（无则不探测）；
// ② GlobalEnabled 关闭时（逃生门）不得调用——本方法由 globalOn(a) 内部兜底，若账号
// 因开关回落 cn 则返回静态名单（handler 侧仍零探测）。
//
// 返回的 Credits 恒为空（PLAN §3.D2：倍率不进 global 路径）。
func (c *Client) FetchGlobalModelInfos(a *auth.Auth) []ModelInfo {
	if !c.globalOn(a) {
		// 逃生门兜底：账号不路由 global 上游 → 不探测，回落静态名单（零上游调用）。
		return staticGlobalModelInfos()
	}

	c.globalModels.Lock()
	if len(c.globalModels.models) > 0 && time.Since(c.globalModels.fetched) < globalModelsTTL {
		out := c.globalModels.models
		c.globalModels.Unlock()
		return out
	}
	if !c.globalModels.lastFail.IsZero() && time.Since(c.globalModels.lastFail) < globalModelsFailCooldown {
		// 负缓存冷却期内：避免反复打上游，直接按失败处理（回落静态）。
		c.globalModels.Unlock()
		return staticGlobalModelInfos()
	}
	c.globalModels.Unlock()

	probed, err := c.probeGlobalModels(a)
	if err != nil || len(probed) == 0 {
		// 探测失败：负缓存 + 回落静态名单。
		c.globalModels.Lock()
		c.globalModels.lastFail = time.Now()
		c.globalModels.models = nil
		c.globalModels.Unlock()
		return staticGlobalModelInfos()
	}

	// 成功：静态名单为基底，追加探测独有（去重），同 id 用探测元数据覆盖。
	merged := mergeGlobalModelInfos(probed)

	c.globalModels.Lock()
	c.globalModels.models = merged
	c.globalModels.fetched = time.Now()
	c.globalModels.lastFail = time.Time{}
	c.globalModels.Unlock()
	return merged
}

// staticGlobalModelInfos 静态名单 → []ModelInfo（仅 ID，元数据留空由调用方兜底）。
func staticGlobalModelInfos() []ModelInfo {
	out := make([]ModelInfo, 0, len(GlobalModelNames))
	for _, id := range GlobalModelNames {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, ModelInfo{ID: id})
		}
	}
	return out
}

// mergeGlobalModelInfos 合并静态名单与探测结果：静态名单定序为基底，同 id 取探测元数据，
// 探测独有追加在尾部。两者都去重（探测内部的重复 id 后者覆盖前者）。
func mergeGlobalModelInfos(probed []ModelInfo) []ModelInfo {
	byID := make(map[string]ModelInfo, len(probed))
	probeOrder := make([]string, 0, len(probed))
	for _, mi := range probed {
		id := strings.TrimSpace(mi.ID)
		if id == "" {
			continue
		}
		mi.ID = id
		if _, dup := byID[id]; !dup {
			probeOrder = append(probeOrder, id)
		}
		byID[id] = mi
	}

	out := make([]ModelInfo, 0, len(GlobalModelNames)+len(probeOrder))
	seen := make(map[string]bool, cap(out))
	for _, id := range GlobalModelNames {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if mi, ok := byID[id]; ok {
			out = append(out, mi) // 静态基底 + 探测元数据
			continue
		}
		out = append(out, ModelInfo{ID: id}) // 静态独有：上游未返回，元数据留空
	}
	for _, id := range probeOrder {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, byID[id])
	}
	return out
}

// probeGlobalModels 按候选路径序列发起一次探测，返回条目列表（未去重、已滤 disabled）。
// 家族端点全部非 2xx（等幂探活）才返回错误。
func (c *Client) probeGlobalModels(a *auth.Auth) ([]ModelInfo, error) {
	var lastErr error
	for _, path := range globalModelsProbePaths {
		infos, err := c.globalModelsOnce(a, path)
		if err != nil {
			lastErr = err
			continue
		}
		return infos, nil
	}
	return nil, lastErr
}

// globalModelsOnce 单端点探测。2xx + 解析出非空名单 → (infos, nil)；否则 (nil, err)。
func (c *Client) globalModelsOnce(a *auth.Auth, path string) ([]ModelInfo, error) {
	url := c.chatBase(a) + path // 按 realm 切 base：global 账号 → global base
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	c.CommonHeaders(req, a) // 共享请求头（Origin/Referer/UA），与 FetchModels 同款
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("global models status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	return parseGlobalModelInfos(raw)
}

// parseGlobalModelInfos 容忍两种形态解析模型目录，产出带窗口 / 能力元数据的条目：
//   - 对象数组（主形态，与 CN /console/enterprises/personal/models 同构）：data.models[]，
//     maxInputTokens→ContextWindow、maxOutputTokens→MaxTokens、maxAllowedSize、
//     supportsReasoning / supportsImages / reasoning.*；id 缺省时回退 name；disabled 剔除；
//   - 窄表：data 为字符串数组 → 仅 ID，元数据留空（窗口由调用方兜底）。
//
// credits（倍率）**恒不解析**（PLAN §3.D2：倍率不进 global 路径）。
// 解析成功但名单为空 → 返回错误（调用方回落静态，等价"该端点没给全"）。
func parseGlobalModelInfos(raw []byte) ([]ModelInfo, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("global models parse: %w", err)
	}
	if codeRaw, ok := envelope["code"]; ok {
		var code int
		if err := json.Unmarshal(codeRaw, &code); err == nil && code != 0 {
			return nil, fmt.Errorf("global models code=%d", code)
		}
	}
	payload := json.RawMessage(raw)
	if data, ok := envelope["data"]; ok && len(strings.TrimSpace(string(data))) > 0 && string(data) != "null" {
		payload = data
	}
	out, err := parseGlobalModelPayload(payload)
	if err != nil || len(out) == 0 {
		if err != nil {
			return nil, fmt.Errorf("global models parse: %w", err)
		}
		return nil, fmt.Errorf("global models empty list")
	}
	return out, nil
}

// parseGlobalModelPayload 兼容国际站不同版本的模型目录：data 直接数组、
// data.models/data.items，以及顶层 models/items。国际站曾在这几种 envelope 之间切换，
// 只支持 data.models 会把登录成功的账号误判为“无模型”。
func parseGlobalModelPayload(raw json.RawMessage) ([]ModelInfo, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return parseGlobalModelValue(value)
}

func parseGlobalModelValue(value any) ([]ModelInfo, error) {
	switch v := value.(type) {
	case []any:
		out := make([]ModelInfo, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				if id := strings.TrimSpace(s); id != "" {
					out = append(out, ModelInfo{ID: id})
				}
				continue
			}
			if obj, ok := item.(map[string]any); ok {
				if mi, ok := parseGlobalModelObject(obj); ok {
					out = append(out, mi)
				}
			}
		}
		return out, nil
	case map[string]any:
		for _, key := range []string{"models", "items", "list", "data", "result"} {
			if nested, ok := v[key]; ok {
				if out, err := parseGlobalModelValue(nested); err == nil && len(out) > 0 {
					return out, nil
				}
			}
		}
	}
	return nil, nil
}

func parseGlobalModelObject(obj map[string]any) (ModelInfo, bool) {
	str := func(key string) string { s, _ := obj[key].(string); return strings.TrimSpace(s) }
	id := str("id")
	if id == "" { id = str("modelId") }
	if id == "" { id = str("model") }
	if id == "" { id = str("name") }
	if id == "" { return ModelInfo{}, false }
	if disabled, ok := obj["disabled"].(bool); ok && disabled { return ModelInfo{}, false }
	int64Value := func(key string) int64 {
		if n, ok := obj[key].(float64); ok { return int64(n) }
		return 0
	}
	mi := ModelInfo{ID: id, Name: str("name"), ContextWindow: int64Value("maxInputTokens"), MaxTokens: int64Value("maxOutputTokens"), MaxAllowedSize: int64Value("maxAllowedSize")}
	if mi.ContextWindow == 0 { mi.ContextWindow = int64Value("contextWindow") }
	if mi.MaxTokens == 0 { mi.MaxTokens = int64Value("maxTokens") }
	mi.SupportsReasoning, _ = obj["supportsReasoning"].(bool)
	mi.SupportsImages, _ = obj["supportsImages"].(bool)

	// reasoning 档位（思考档位选择依赖它）：与 CN 目录同款字段，
	// defaultEffort 新键优先、effort 老键兜底，supportedEfforts 是可选档位集合。
	if r, ok := obj["reasoning"].(map[string]any); ok {
		reasonStr := func(key string) string {
			s, _ := r[key].(string)
			return strings.TrimSpace(s)
		}
		mi.DefaultEffort = reasonStr("defaultEffort")
		if mi.DefaultEffort == "" {
			mi.DefaultEffort = reasonStr("effort")
		}
		mi.CanDisableThinking, _ = r["canDisableThinking"].(bool)
		if arr, ok := r["supportedEfforts"].([]any); ok {
			for _, item := range arr {
				if s, ok := item.(string); ok {
					if s = strings.TrimSpace(s); s != "" {
						mi.Efforts = append(mi.Efforts, s)
					}
				}
			}
		}
	}
	// Credits 故意留空：PLAN §3.D2，倍率不进入 global 路径。
	return mi, true
}
