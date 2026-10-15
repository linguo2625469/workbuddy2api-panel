// chatheaders.go — the chat request header set, ported field-by-field from
// the live-confirmed
// reference. The transport is plain net/http (chathttp.go).

package upstream

import (
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/zcode/store"
	"github.com/linguo2625469/workbuddy2api-panel/internal/zcode/util"
)

// chatRequestHeaders builds the header set for the chat request — the
// identity set.
//
// JWT (start-plan) channel rules from the reference:
//   - x-api-key, x-query-id, x-session-id are NOT sent — the official
//     start-plan client omits them; sending x-query-id/x-session-id here
//     triggers upstream 3012 "unusual activity" (empirically).
//   - trace trio (x-request-id, x-zcode-session-type, x-zcode-trace-id)
//     is regenerated per request.
//   - X-Device-Mid is the account's own persisted uuid4 ("a fresh install
//     on this machine" per account), not a shared/derived id.
//   - the captcha region rides with the minted token, never hardcoded.
func chatRequestHeaders(jwt, appVer, captcha, captchaRegion, deviceMid string, extra http.Header) [][2]string {
	h := [][2]string{}
	add := func(k, v string) {
		if v != "" {
			h = append(h, [2]string{k, v})
		}
	}
	// anthropic-beta passes through from the client when present (the
	// reference's drop-list keeps it)
	if extra != nil {
		if v := extra.Get("anthropic-beta"); v != "" {
			add("anthropic-beta", v)
		}
	}
	add("content-type", "application/json")
	add("Authorization", "Bearer "+jwt)
	add("anthropic-version", "2023-06-01")
	// identity set (identity.py order)
	add("HTTP-Referer", "https://zcode.z.ai/")
	add("User-Agent", "ZCode/"+appVer)
	add("X-ZCode-App-Version", appVer)
	add("X-Title", "Z Code@cli")
	add("X-ZCode-Agent", "glm")
	add("X-Platform", "win32-x64")
	add("X-Release-Channel", "production")
	add("X-Client-Language", "en-001")
	add("X-Client-Timezone", "Europe/Bucharest")
	add("X-Os-Category", "windows")
	add("X-Os-Version", "10.0.26200")
	if deviceMid == "" {
		deviceMid = store.DeviceMidFor(jwt)
	}
	add("X-Device-Mid", deviceMid)
	// trace trio: start-plan sends ONLY these (no x-query-id / x-session-id)
	add("x-request-id", util.GenerateUUID())
	add("x-zcode-session-type", "main")
	add("x-zcode-trace-id", util.GenerateUUID())
	if captcha != "" {
		add("X-Aliyun-Captcha-Verify-Param", captcha)
		if captchaRegion != "" {
			add("X-Aliyun-Captcha-Verify-Region", captchaRegion)
		}
	}
	// httpx's own defaults (what the reference adds beyond the explicit set)
	add("accept", "*/*")
	add("accept-encoding", "gzip, deflate")
	return h
}
