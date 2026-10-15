package upstream

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
	"github.com/linguo2625469/workbuddy2api-panel/internal/zcode/store"
)

const QuotaCode = 1005

// Norm normalizes a model id for dead-mark/match comparisons.
func Norm(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func originJSONCode(body []byte) int {
	raw := bytesTrimLeft(body)
	if len(raw) == 0 || raw[0] != '{' {
		return 0
	}
	var data struct {
		Code any `json:"code"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return 0
	}
	switch v := data.Code.(type) {
	case float64:
		return int(v)
	case string:
		n := 0
		for _, c := range v {
			if c < '0' || c > '9' {
				return 0
			}
			n = n*10 + int(c-'0')
		}
		return n
	default:
		return 0
	}
}

func IsQuotaError(body []byte) bool {
	return originJSONCode(body) == QuotaCode
}

const CaptchaFailCode = 3007

func IsCaptchaError(body []byte) bool {
	return originJSONCode(body) == CaptchaFailCode
}

// QuotaFamilyCodes — the Z.AI 429 family meaning quota/usage caps (docs.z.ai
// error table): 1113 arrears; 1308 usage cap; 1309 plan
// expired; 1310 weekly/monthly cap; 1311 plan lacks the model; 1313 fair
// use; 1316-1321 5h/7d caps and sub/enterprise caps. Distinct from the
// transient 429s (1302/1305) — these mean the MODEL is exhausted.
var QuotaFamilyCodes = map[int]bool{
	1113: true, 1308: true, 1309: true, 1310: true, 1311: true, 1313: true,
	1316: true, 1317: true, 1318: true, 1319: true, 1320: true, 1321: true,
}

// IsQuotaFamily429: HTTP 429 whose body code is a quota cap → the account's
// model is exhausted (mark dead for the model, switch account — not a proxy
// rotation and not a cooldown).
func IsQuotaFamily429(status int, body []byte) bool {
	return status == http.StatusTooManyRequests && QuotaFamilyCodes[originJSONCode(body)]
}

// IsAdmissionLimit: 3010 model-admission concurrency limit — the account
// stays usable; the official client waits 1s then 2s and retries the SAME
// account (busy-retry on the same egress).
func IsAdmissionLimit(status int, body []byte) bool {
	if status != http.StatusTooManyRequests {
		return false
	}
	return originJSONCode(body) == 3010
}

func IsSSE(body []byte) bool {
	raw := bytesTrimLeft(body)
	return strings.HasPrefix(string(raw), "event:") || strings.HasPrefix(string(raw), "data:")
}

func ParseModel(body []byte) string {
	var data struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &data)
	return data.Model
}

func FracFromEntitlements(ents []map[string]any, model string) (float64, bool) {
	want := Norm(model)
	for _, e := range ents {
		name := Str(e["name"])
		if want != "" && Norm(name) != want {
			continue
		}
		if pe := int64(AsInt(e["period_end"])); pe > 0 && pe <= time.Now().Unix() {
			return 1, true
		}
		rem := AsInt(e["remaining"])
		tot := AsInt(e["limit"])
		if tot > 0 {
			return float64(rem) / float64(tot), true
		}
		if rem > 0 {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func PeriodEndFromEntitlements(ents []map[string]any, model string) int64 {
	want := Norm(model)
	for _, e := range ents {
		if want != "" && Norm(Str(e["name"])) != want {
			continue
		}
		return int64(AsInt(e["period_end"]))
	}
	return 0
}

func DeadUntilUnix(ents []map[string]any, model string) int64 {
	now := time.Now().Unix()
	if pe := PeriodEndFromEntitlements(ents, model); pe > now {
		return pe
	}
	return now + 86400
}

func IsEmptyGrant(q store.Quota) bool {
	if q.Remaining > 0 || q.Limit > 0 || len(q.Entitlements) > 0 {
		return false
	}
	p := strings.TrimSpace(q.PlanName)
	return p == "" || strings.EqualFold(p, "Free")
}

func OverloadedJSON() []byte {
	return []byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
}

func CaptchaRequiredJSON() []byte {
	return []byte(`{"type":"error","error":{"type":"captcha_required","message":"captcha solve failed; retry later"}}`)
}

func BodySnip(body []byte, n int) string {
	if n <= 0 {
		n = 400
	}
	raw := body
	if len(raw) > n {
		raw = raw[:n]
	}
	s := strings.ReplaceAll(string(raw), "\r", " ")
	s = strings.ReplaceAll(s, "\n", "\\n")
	var b strings.Builder
	for _, r := range s {
		if r < 128 {
			b.WriteRune(r)
		} else {
			b.WriteByte('?')
		}
	}
	out := b.String()
	if len(body) > n {
		out += " … " + strconv.Itoa(len(body)) + "b"
	}
	if out == "" {
		return "-"
	}
	return out
}

func bytesTrimLeft(b []byte) []byte {
	i := 0
	for i < len(b) && (b[i] == ' ' || b[i] == '\n' || b[i] == '\r' || b[i] == '\t') {
		i++
	}
	return b[i:]
}

// Str — narrow any-to-string (JSON scalars).
func Str(v any) string {
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

// AsInt — narrow any-to-int (JSON scalars).
func AsInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return 0
	}
}
