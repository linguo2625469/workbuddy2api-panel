package upstream

import (
	"net/http"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestAPIKeyChatHeadersDoubleAuth 静态 API Key 账号 chat 出站：Authorization: Bearer
// 之外再注入 X-API-Key（官方 CLI 双头行为）。对照 OAuth 个人号不发 X-API-Key。
func TestAPIKeyChatHeadersDoubleAuth(t *testing.T) {
	c := &Client{}
	const key = "ck_9f3a2b1c.deadbeef"

	req, _ := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	c.ChatHeaders(req, &auth.Auth{UID: "u1", AccessToken: key}, "", ChatMeta{ConversationRequestID: "abc"})
	if got := req.Header.Get("Authorization"); got != "Bearer "+key {
		t.Errorf("Authorization=%q want Bearer <key>", got)
	}
	if got := req.Header.Get("X-API-Key"); got != key {
		t.Errorf("X-API-Key=%q want %q", got, key)
	}
	// 静态 Key 无企业身份：不得注入 X-Enterprise-Id。
	if got := req.Header.Get("X-Enterprise-Id"); got != "" {
		t.Errorf("API Key 账号不应注入 X-Enterprise-Id，实际=%q", got)
	}

	// 对照：OAuth 个人号不发 X-API-Key。
	req2, _ := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	c.ChatHeaders(req2, &auth.Auth{UID: "u2", AccessToken: "at"}, "", ChatMeta{ConversationRequestID: "def"})
	if got := req2.Header.Get("X-API-Key"); got != "" {
		t.Errorf("OAuth 账号不应注入 X-API-Key，实际=%q", got)
	}
}

// TestAPIKeyBillingHeadersNoEnterprise API Key 走企业口径额度端点时不带企业身份头
// （实测不需要，带上反而暴露虚假身份）。
func TestAPIKeyBillingHeadersNoEnterprise(t *testing.T) {
	c := &Client{}
	req, _ := http.NewRequest(http.MethodPost, "https://www.codebuddy.cn/v2/billing/meter/get-enterprise-user-usage", nil)
	c.BillingHeaders(req, &auth.Auth{UID: "u1", AccessToken: "ck_9f3a2b1c.deadbeef"})
	if got := req.Header.Get("X-Enterprise-Id"); got != "" {
		t.Errorf("X-Enterprise-Id=%q want empty", got)
	}
	if got := req.Header.Get("X-Tenant-Id"); got != "" {
		t.Errorf("X-Tenant-Id=%q want empty", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer ck_9f3a2b1c.deadbeef" {
		t.Errorf("Authorization=%q", got)
	}
}
