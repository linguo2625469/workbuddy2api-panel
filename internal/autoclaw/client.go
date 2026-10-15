// Package autoclaw 桥接独立部署的 autoclaw2api 服务（Python，autoclaw.z.ai
// 云端沙箱网关）。本包只做三件事：健康探测、模型列表拉取（并转换 owned_by
// 供统一列表合并）、OpenAI 兼容反向代理（含 SSE 流式透传）。账号池/状态机/
// 签到全部由 autoclaw2api 自治，Go 侧不做重复实现。
package autoclaw

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// Client autoclaw2api 的最小客户端。
type Client struct {
	// BaseURL autoclaw2api 服务基址，如 http://autoclaw2api:7865。
	BaseURL string
	// APIKey autoclaw2api 的 api_key（与主网关统一密钥一致）。
	APIKey string
	// HTTP 共享客户端（不跟随主链路超时：沙箱唤醒 + agent 流可能很慢）。
	HTTP *http.Client
}

// New 构造客户端。
func New(baseURL, apiKey string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: 150 * time.Second},
	}
}

// Healthy autoclaw2api 是否可达且至少有一个健康账号。
func (c *Client) Healthy() bool {
	if c == nil || c.BaseURL == "" {
		return false
	}
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var st struct {
		Healthy int `json:"healthy"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&st) != nil {
		return false
	}
	return st.Healthy > 0
}

// Models 拉取 autoclaw2api 的模型列表，把 owned_by 归一为 "autoclaw-glm"
// （unifiedModelList 按规范化裸名与 cn:/trae/zcode 条目同名合并，owned_by
// 拼接出 "…+autoclaw-glm" 多池标记）。
func (c *Client) Models() []map[string]any {
	if c == nil || c.BaseURL == "" {
		return nil
	}
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+"/v1/models", nil)
	if err != nil {
		return nil
	}
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out) != nil {
		return nil
	}
	for _, m := range out.Data {
		// zai_glm-5.3-flash 与上游裸名 glm-5.3-flash 是同一模型（cn 通道两个
		// 别名都暴露），统一重命名为裸名，与 cn:/zcode/workbuddy 同名条目合并，
		// 避免统一列表出现重复行。
		if id, _ := m["id"].(string); strings.EqualFold(id, "zai_glm-5.3-flash") {
			m["id"] = "glm-5.3-flash"
		}
		m["owned_by"] = "autoclaw-glm"
	}
	return out.Data
}

// KnownSet 返回 autoclaw 目录的模型 id 集合（含 openclaw/zai_auto 等 agent
// 名）。map 键为原 id 与小写形式，供 KnownModel 双重匹配。
func (c *Client) KnownSet() map[string]bool {
	set := map[string]bool{}
	for _, m := range c.Models() {
		if id, _ := m["id"].(string); id != "" {
			set[id] = true
			set[strings.ToLower(id)] = true
		}
	}
	// 旧名兼容：zai_glm-5.3-flash 在桥接层已并入 glm-5.3-flash，但显式请求
	// 旧名仍要能路由（上游本身认识旧 id，转发原样透传即可）。
	set["zai_glm-5.3-flash"] = true
	return set
}

// KnownModel 判断 model 是否 autoclaw 目录已知模型（精确或小写匹配）。
// 缓存 60s：模型目录变化极慢，避免每次 chat 都打一次上游。
func (c *Client) KnownModel(model string, cache *KnownCache) bool {
	if c == nil {
		return false
	}
	if cache != nil {
		if set, ok := cache.get(); ok {
			return set[strings.ToLower(model)]
		}
		set := c.KnownSet()
		if cache != nil {
			cache.put(set)
		}
		return set[strings.ToLower(model)]
	}
	return c.KnownSet()[strings.ToLower(model)]
}

// KnownCache KnownSet 的 60 秒 TTL 缓存。
type KnownCache struct {
	ttl time.Duration
	mu  chan struct{}
	set map[string]bool
	at  time.Time
}

// NewKnownCache 构造缓存。
func NewKnownCache() *KnownCache {
	return &KnownCache{ttl: 60 * time.Second, mu: make(chan struct{}, 1)}
}

func (k *KnownCache) get() (map[string]bool, bool) {
	k.mu <- struct{}{}
	defer func() { <-k.mu }()
	return k.set, k.set != nil && time.Since(k.at) < k.ttl
}

func (k *KnownCache) put(set map[string]bool) {
	k.mu <- struct{}{}
	defer func() { <-k.mu }()
	k.set, k.at = set, time.Now()
}

// Proxy 返回把 /v1/chat/completions（及未来 /v1/*）原样转发到 autoclaw2api
// 的反向代理：注入 Authorization，透传流式响应。
func (c *Client) Proxy() http.Handler {
	target, err := url.Parse(c.BaseURL)
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "autoclaw: bad base url", http.StatusBadGateway)
		})
	}
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.FlushInterval = 0 // 逐字节透传 SSE
	orig := rp.Director
	rp.Director = func(req *http.Request) {
		orig(req)
		req.Host = target.Host
		if c.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.APIKey)
		}
	}
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"message":"autoclaw upstream unreachable: ` +
			strings.ReplaceAll(err.Error(), `"`, "'") + `","type":"api_error","code":"bad_gateway"}}`))
	}
	return rp
}
