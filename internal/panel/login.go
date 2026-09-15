// login.go 面板内嵌的 WorkBuddy CN OAuth 设备授权流程（cmd/login 的进程内移植）。
//
//	POST /panel/api/login/start → 拿 state+authUrl，state 存进程内（不再落 /tmp，
//	  原方案在 Windows 上不可用），返回授权 URL；
//	GET  /panel/api/login/poll   → 面板前端每 3s 轮询本接口；未完成返回 done=false，
//	  完成后取 uid/nickname、凭证落盘 auths/workbuddy-<uid>.json、热加载进池
//	  （pool.Add + Revive），并顺带签到 + 余额刷新 —— 免重启加载新账号。
//
// 无 PKCE（workbuddy 设备流由服务端签发 state），请求头与上游端点与 cmd/login 保持一致。
package panel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const (
	upstreamBaseCN    = "https://copilot.tencent.com"
	clientUA          = "CLI/2.63.2 CodeBuddy/2.63.2"
	originReferer     = "https://www.codebuddy.cn"
	endpointAuthState = upstreamBaseCN + "/v2/plugin/auth/state?platform=CLI"
	endpointLoginAcct = upstreamBaseCN + "/v2/plugin/login/account?state="
	endpointAuthToken = upstreamBaseCN + "/v2/plugin/auth/token?state="
)

// loginHTTP 设备授权专用 client：短超时、无 cookie（每请求携带 state，无会话态）。
var loginHTTP = &http.Client{Timeout: 30 * time.Second}

func commonHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", originReferer)
	req.Header.Set("Referer", originReferer+"/")
	req.Header.Set("User-Agent", clientUA)
}

// validUID 校验上游返回的 uid 是否可安全用于拼文件名。
// 只放行字母、数字、下划线、连字符（腾讯侧 uid 实测为 UUID 形态），
// 长度上限 64 兜底异常超长串；拒绝 . / \ 等路径字符与空串。
func validUID(uid string) bool {
	if uid == "" || len(uid) > 64 {
		return false
	}
	for _, c := range uid {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// apiEnvelope 与 upstream 同形：{code,msg,data}，code!=0 视为业务错误。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// doJSON 发一次 JSON 请求并解信封。
func doJSON(method, fullURL string, bearer string, body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	commonHeaders(req)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := loginHTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

// loginStart 发起设备授权：POST auth/state 拿授权 URL。
func (p *Panel) loginStart(w http.ResponseWriter, r *http.Request) {
	data, status, err := doJSON(http.MethodPost, endpointAuthState, "", bytes.NewReader([]byte("{}")))
	if err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("auth state (upstream %d): %v", status, err))
		return
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		writeErr(w, http.StatusBadGateway, "auth state: missing state or authUrl")
		return
	}
	p.loginMu.Lock()
	// 顺手回收过期会话，防"开弹窗走开"的 state 滞留。
	for s, created := range p.logins {
		if time.Since(created) > loginTTL {
			delete(p.logins, s)
		}
	}
	p.logins[st.State] = time.Now()
	p.loginMu.Unlock()
	log.Printf("panel: 发起 OAuth 添加账号（state=%s...）", st.State[:min(8, len(st.State))])
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "url": st.AuthURL, "state": st.State})
}

// loginPoll 轮询登录态。未完成 → {done:false}；完成 → 建凭证、落盘、热加载、签到。
func (p *Panel) loginPoll(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state == "" {
		writeErr(w, http.StatusBadRequest, "missing state")
		return
	}
	p.loginMu.Lock()
	_, known := p.logins[state]
	p.loginMu.Unlock()
	if !known {
		writeErr(w, http.StatusNotFound, "unknown or expired state（请重新发起添加账号）")
		return
	}

	// auth/token 是权威登录状态端点：pending 时业务 code 非 0（"login ing"）。
	tokRaw, _, err := doJSON(http.MethodGet, endpointAuthToken+state, "", nil)
	if err != nil {
		// pending / 未完成：面板前端继续轮询。
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": err.Error()})
		return
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "waiting for login"})
		return
	}

	// 完成：取 uid/nickname（失败不阻塞，仅缺展示名）。
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	if acctRaw, _, err := doJSON(http.MethodGet, endpointLoginAcct+state, tok.AccessToken, nil); err == nil {
		_ = json.Unmarshal(acctRaw, &acct)
	}
	if acct.UID == "" {
		writeErr(w, http.StatusBadGateway, "login done but no uid（token 已发但账号信息获取失败，请重试）")
		return
	}
	// UID 来自上游响应，未经校验就用于拼文件名会被路径穿越利用
	// （filepath.Join("./auths", "workbuddy-../../evil.json") → auths/evil.json）。
	// UID 是腾讯侧账号标识，实测为 UUID（十六进制与连字符），故只放行 [A-Za-z0-9_-]。
	if !validUID(acct.UID) {
		writeErr(w, http.StatusBadGateway, "上游返回的 uid 含非法字符，拒绝落盘（防路径穿越）")
		return
	}

	// 凭证落盘（嵌套形，与 auths/ 目录既有格式一致）→ 热加载进池。
	if err := os.MkdirAll(p.cfg.AuthDir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "mkdir auth dir: "+err.Error())
		return
	}
	a := &auth.Auth{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
		Domain:       tok.Domain,
		UID:          acct.UID,
		EnterpriseID: acct.EnterpriseID,
		Nickname:     acct.Nickname,
		FilePath:     filepath.Join(p.cfg.AuthDir, fmt.Sprintf("workbuddy-%s.json", acct.UID)),
	}
	if err := a.SaveAtomic(); err != nil {
		writeErr(w, http.StatusInternalServerError, "save auth: "+err.Error())
		return
	}
	p.cfg.Pool.Add(a)
	p.cfg.Pool.Revive(acct.UID) // 全新登录 = 人工恢复口径：清掉旧号遗留的禁用/冷却/熔断

	// 顺带签到 + 余额刷新（幂等；失败不影响登录结果，只体现在返回字段里）。
	checkinMsg := ""
	if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
		checkinMsg = err.Error()
		// 幂等"今天已签到"同样记入签到日（供账号池展示）。
		if upstream.IsAlreadyCheckin(err) {
			p.cfg.Pool.NoteCheckin(acct.UID)
		}
	} else {
		p.cfg.Pool.NoteCheckin(acct.UID)
	}
	remain := int64(-1)
	total := int64(0)
	if rm, tt, err := p.cfg.Upstream.UserResource(a); err == nil {
		remain, total = rm, tt
		p.cfg.Pool.ReenableIfCredits(acct.UID, rm, tt)
	}

	p.loginMu.Lock()
	delete(p.logins, state)
	p.loginMu.Unlock()
	log.Printf("panel: 新账号已热加载 uid=%s nickname=%q（免重启生效）", acct.UID, acct.Nickname)
	writeJSON(w, http.StatusOK, map[string]any{
		"done":            true,
		"uid":             acct.UID,
		"nickname":        acct.Nickname,
		"credits":         remain,
		"credits_total":   total,
		"checkin_message": checkinMsg,
	})
}
