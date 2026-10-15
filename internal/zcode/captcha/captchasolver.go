package captcha

// captchasolver.go — captcha scene/region/prefix config: live from the
// origin client/configs (5-minute cache), cn fallback ("旧值 sgp 已过期" —
// sgp is expired, the live-measured region is cn). The client/configs
// fetch below is THE single fetch of that endpoint in the codebase: the
// captcha section parsed here, the model metadata parsed by the gateway
// catalog (models.go). Consumed by the node solver (nodesolver.go);
// earlier mint backends live in git history.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/zcode/config"
)

var ErrCaptchaUnavailable = errors.New("captcha solver unavailable")

// defaultCaptchaCfg — both references' fallback.
var defaultCaptchaCfg = captchaCfg{Enabled: true, Prefix: "no8xfe", Region: "cn", SceneID: "11xygtvd"}

type captchaCfg struct {
	Enabled bool
	Prefix  string
	Region  string
	SceneID string
}

// ClientConfigs fetches the origin client/configs document (5-minute
// cache, direct egress, no auth — the endpoint is public). Returns nil
// when the fetch fails and no cached copy exists.
func ClientConfigs(ctx context.Context) []byte {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	if time.Since(cfgAt) < 5*time.Minute && cfgRaw != nil {
		return cfgRaw
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		config.OriginBaseURL+"/api/v1/client/configs?app_version="+url.QueryEscape(config.ClientVersion), nil)
	if err != nil {
		return cfgRaw
	}
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: nil}}
	if resp, err := client.Do(req); err == nil {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if len(body) > 0 {
			cfgRaw, cfgAt = body, time.Now()
		}
	}
	return cfgRaw
}

var (
	cfgMu  sync.Mutex
	cfgRaw []byte
	cfgAt  time.Time
)

// fetchCaptchaCfg parses the captcha section of client/configs, falling
// back to the baked defaults when there is no data (dead egress, mitm
// CA, ...).
func fetchCaptchaCfg(ctx context.Context) captchaCfg {
	c := defaultCaptchaCfg
	body := ClientConfigs(ctx)
	if body == nil {
		return c
	}
	var wrap struct {
		Code int `json:"code"`
		Data struct {
			Configs struct {
				Captcha struct {
					Enabled *bool  `json:"enabled"`
					Prefix  string `json:"prefix"`
					Region  string `json:"region"`
					SceneID string `json:"sceneId"`
				} `json:"captcha"`
			} `json:"configs"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &wrap) != nil || wrap.Code != 0 {
		return c
	}
	cc := wrap.Data.Configs.Captcha
	if cc.Prefix != "" {
		c = captchaCfg{Enabled: cc.Enabled == nil || *cc.Enabled, Prefix: cc.Prefix, Region: cc.Region, SceneID: cc.SceneID}
	}
	return c
}
