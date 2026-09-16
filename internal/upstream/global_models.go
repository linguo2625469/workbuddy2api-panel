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
	var env struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("global models parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("global models code=%d", env.Code)
	}
	trimmed := strings.TrimSpace(string(env.Data))
	if strings.HasPrefix(trimmed, "[") {
		// 窄表形态：data 为字符串数组。
		var arr []string
		if err := json.Unmarshal(env.Data, &arr); err != nil {
			return nil, fmt.Errorf("global models parse (narrow): %w", err)
		}
		out := make([]ModelInfo, 0, len(arr))
		for _, id := range arr {
			if id = strings.TrimSpace(id); id != "" {
				out = append(out, ModelInfo{ID: id})
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("global models empty list")
		}
		return out, nil
	}
	// 对象形态：data.models[]，字段名与 CN 目录一致（maxInputTokens/maxOutputTokens/…）。
	var obj struct {
		Models []struct {
			ID                string   `json:"id"`
			Name              string   `json:"name"`
			MaxInputTokens    int64    `json:"maxInputTokens"`
			MaxOutputTokens   int64    `json:"maxOutputTokens"`
			MaxAllowedSize    int64    `json:"maxAllowedSize"`
			Disabled          bool     `json:"disabled"`
			SupportsReasoning bool     `json:"supportsReasoning"`
			SupportsImages    bool     `json:"supportsImages"`
			Reasoning         struct {
				Effort             string   `json:"effort"`        // 老模型键
				DefaultEffort      string   `json:"defaultEffort"` // 新模型键
				CanDisableThinking bool     `json:"canDisableThinking"`
				SupportedEfforts   []string `json:"supportedEfforts"`
			} `json:"reasoning"`
		} `json:"models"`
	}
	if err := json.Unmarshal(env.Data, &obj); err != nil {
		return nil, fmt.Errorf("global models parse: %w", err)
	}
	out := make([]ModelInfo, 0, len(obj.Models))
	for _, m := range obj.Models {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			id = strings.TrimSpace(m.Name)
		}
		if id == "" || m.Disabled {
			continue
		}
		def := m.Reasoning.Effort
		if def == "" {
			def = m.Reasoning.DefaultEffort // 新旧双键兼容，与 CN 侧同款
		}
		out = append(out, ModelInfo{
			ID:                 id,
			Name:               m.Name,
			ContextWindow:      m.MaxInputTokens,
			MaxTokens:          m.MaxOutputTokens,
			MaxAllowedSize:     m.MaxAllowedSize,
			Efforts:            m.Reasoning.SupportedEfforts,
			DefaultEffort:      def,
			CanDisableThinking: m.Reasoning.CanDisableThinking,
			SupportsReasoning:  m.SupportsReasoning,
			SupportsImages:     m.SupportsImages,
			// Credits 故意留空：PLAN §3.D2，倍率不进入 global 路径。
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("global models empty list")
	}
	return out, nil
}
