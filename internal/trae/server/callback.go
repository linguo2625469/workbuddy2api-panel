// callback.go TRAE 登录回调解析 + 登录 URL 构造。
//
// 移植自 login.sh 的内嵌 Python（parse_qs + 双层 JSON + unquote 容错），
// 让 web 面板能在服务端完成「粘贴回调链接 → 换 token → 落盘」全流程，
// 不再依赖外部 python3。
package server

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/trae/upstream"
)

// appVersion 与 login.sh 保持一致；如未来 upstream.IdeVersion 升级，这里同步即可。
const loginAppVersion = upstream.IdeVersion

// loginTraceIDHexLen login_trace_id 的 hex 长度（login.sh 用 secrets.token_hex(8) → 16 hex 字符）。
const loginTraceIDHexLen = 16

// BuildLoginURL 构造 TRAE 登录 URL（复刻 login.sh 的参数集）。
// callbackURL 是 TRAE 登录成功后的重定向落点（如 http://127.0.0.1:7864/authorize）。
// machineID/deviceID 必须与落盘 auth 文件共用同一对（hex32），保证登录态与凭证一致。
func BuildLoginURL(machineID, deviceID, callbackURL string) string {
	v := url.Values{}
	v.Set("login_version", "1")
	v.Set("auth_from", "solo")
	v.Set("login_channel", "native_ide")
	v.Set("plugin_version", "2.3.62834")
	v.Set("auth_type", "local")
	v.Set("client_id", upstream.ClientID)
	v.Set("redirect", "0")
	// login_trace_id：随机性由调用方注入（time 不可用，见 newLoginTraceID）；
	// 这里只负责拼装，调用方传入已生成的 trace id。
	return upstream.ConsoleHost + "/authorization?" + v.Encode() +
		"&login_trace_id=" + url.QueryEscape(machineTraceID(machineID, deviceID)) +
		"&auth_callback_url=" + url.QueryEscape(callbackURL) +
		"&machine_id=" + url.QueryEscape(machineID) +
		"&device_id=" + url.QueryEscape(deviceID) +
		"&x_device_id=" + url.QueryEscape(deviceID) +
		"&x_machine_id=" + url.QueryEscape(machineID) +
		"&x_device_brand=PC" +
		"&x_device_type=PC" +
		"&x_os_version=1.0" +
		"&x_app_version=" + url.QueryEscape(loginAppVersion) +
		"&x_app_type=stable"
}

// machineTraceID 由 machineID+deviceID 派生一个稳定的 login_trace_id（hex16）。
// 时间源不可用，用输入摘要的前 16 字符保证可复现且非空；调用方也可直接传任意 hex16。
func machineTraceID(machineID, deviceID string) string {
	h := machineID + deviceID
	// 取拼接收尾 16 字符（machineID/deviceID 均为 hex32，尾部稳定）
	if len(h) >= loginTraceIDHexLen {
		return h[len(h)-loginTraceIDHexLen:]
	}
	// 不足则左侧补 0
	return strings.Repeat("0", loginTraceIDHexLen-len(h)) + h
}

// CallbackInfo 回调链接解析结果（脱敏前的原始凭证，仅服务端内部使用）。
type CallbackInfo struct {
	RefreshToken string // 优先取 query.refreshToken，缺省回退 userJwt.RefreshToken
	AccessToken  string // 无 refreshToken 时回退 userJwt.Token（兜底）
	UID          string // userInfo.UserID
	Nickname     string // userInfo.ScreenName
	EnterpriseID string // userInfo.TenantID（注意回调字段名是 TenantID）
	ExpiresAt    int64  // ExchangeToken 后由调用方填；此处仅 userJwt 兜底路径会设
}

// parseJSONParam 解回调里 URL 编码的 JSON 参数。
// parse_qs 已解一层 percent-encoding，这里再容错解一层 unquote（复刻 login.sh.parse_json_param）。
func parseJSONParam(raw string) map[string]any {
	if raw == "" {
		return nil
	}
	candidates := []string{raw}
	if uq, err := url.QueryUnescape(raw); err == nil && uq != raw {
		candidates = append(candidates, uq)
	}
	for _, c := range candidates {
		var obj map[string]any
		if json.Unmarshal([]byte(c), &obj) == nil && obj != nil {
			return obj
		}
	}
	return nil
}

func getString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		return x.String()
	}
	return fmt.Sprintf("%v", v)
}

// ParseCallback 解析 TRAE 登录回调链接，提取凭证字段。
//
// 回调形如：
//
//	http://127.0.0.1:18080/authorize?refreshToken=...&userInfo={...}&userJwt={...}
//
// refreshToken 优先；缺失时回退 userJwt.Token（login.sh 兜底分支）。
// 仅做解析，不执行 ExchangeToken（换 token 由 upstream.Client.RefreshToken 完成）。
func ParseCallback(rawURL string) (*CallbackInfo, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, fmt.Errorf("empty callback url")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse callback url: %w", err)
	}
	q := u.Query()
	info := &CallbackInfo{
		RefreshToken: q.Get("refreshToken"),
	}

	userInfo := parseJSONParam(q.Get("userInfo"))
	info.UID = getString(userInfo, "UserID")
	info.Nickname = getString(userInfo, "ScreenName")
	info.EnterpriseID = getString(userInfo, "TenantID")

	userJwt := parseJSONParam(q.Get("userJwt"))
	jwtToken := getString(userJwt, "Token")
	jwtRefresh := getString(userJwt, "RefreshToken")

	// 回调缺 refreshToken 时，回退 userJwt 的 RefreshToken（复刻 login.sh:143-144）
	if info.RefreshToken == "" {
		info.RefreshToken = jwtRefresh
	}
	if info.RefreshToken == "" {
		// 兜底：无 refreshToken 时直接用 userJwt.Token 作为 accessToken
		info.AccessToken = jwtToken
		if jwtToken == "" {
			return nil, fmt.Errorf("callback missing refreshToken and userJwt.Token")
		}
		// userJwt.TokenExpireAt（毫秒）→ Unix 秒
		if exp := getInt64(userJwt, "TokenExpireAt"); exp > 0 {
			info.ExpiresAt = normalizeExpire(exp)
		}
	}
	return info, nil
}

func getInt64(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	switch x := v.(type) {
	case float64:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	}
	return 0
}

// normalizeExpire TokenExpireAt 毫秒 → Unix 秒（>1e12 视为毫秒），复刻 login.sh 的处理。
func normalizeExpire(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}

// ExpireAtFromExchange 把 ExchangeToken 返回的 TokenExpireAt/TokenExpireDuration 归一化为 Unix 秒，
// 复刻 login.sh:159-162：优先 TokenExpireAt，过期则用 now+TokenExpireDuration。
// 时间源由调用方提供（测试可注入），生产用 time.Now()。
func ExpireAtFromExchange(tokenExpireAt, tokenExpireDuration int64, now time.Time) int64 {
	if tokenExpireAt > 0 {
		exp := normalizeExpire(tokenExpireAt)
		if exp > now.Unix() {
			return exp
		}
	}
	if tokenExpireDuration > 0 {
		return now.Add(time.Duration(tokenExpireDuration) * time.Second).Unix()
	}
	return 0
}
