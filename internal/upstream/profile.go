// profile.go Web 控制台账号资料（issue #94：改名后免重登同步昵称）。
//
// GET {webBase}/console/account，Bearer + x-client-platform: web（与 tasks/claim
// 同一鉴权形态，实测 2026-10-01：无需 Web 会话 cookie）。
//
// 隐私边界（重要）：该接口响应包含手机号（phoneNumber）等个人敏感信息。本方法
// 只解析 nickname 与 uid（uid 仅做一致性核对），其余字段一概不解析、不落日志、
// 不透传——调用方也拿不到。昵称同步只在面板手动「刷新」时触发，后台余额定时
// 刷新不调用本接口。
package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// FetchAccountProfile 拉取账号资料并返回最新昵称。uid 与凭证不一致时报错
// （防串号）；业务/网络错误原样返回，调用方静默跳过即可。
func (c *Client) FetchAccountProfile(a *auth.Auth) (string, error) {
	req, err := http.NewRequest(http.MethodGet, c.webBase(a)+"/console/account", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("x-client-platform", "web")
	req.Header.Set("Origin", "https://www.workbuddy.cn")
	req.Header.Set("Referer", "https://www.workbuddy.cn/profile/account-settings")
	if ua := c.userAgent(a); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	c.injectAccountStableHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return "", err
	}
	// 只取两个字段：敏感信息（手机号等）在这里就被丢弃，不进入任何后续路径。
	var resp struct {
		UID      string `json:"uid"`
		Nickname string `json:"nickname"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("profile parse: %w", err)
	}
	if resp.UID != "" && a.UID != "" && resp.UID != a.UID {
		return "", fmt.Errorf("profile uid mismatch: resp=%s auth=%s", resp.UID, a.UID)
	}
	return resp.Nickname, nil
}
