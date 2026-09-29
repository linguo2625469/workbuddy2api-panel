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
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	upstreamBaseCN      = "https://copilot.tencent.com"
	upstreamBaseGlobal  = "https://www.workbuddy.ai"
	clientUA            = "CLI/2.63.2 CodeBuddy/2.63.2"
	originRefererCN     = "https://www.codebuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"
	phoneLoginBase      = "https://www.codebuddy.cn"
	phoneLoginUA        = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"
	phoneLoginTTL       = 5 * time.Minute
)

var (
	phoneActionRE = regexp.MustCompile(`(?i)<form[^>]+action=["']([^"']+)["']`)
	phoneRE       = regexp.MustCompile(`^1\d{10}$`)
	phoneCodeRE   = regexp.MustCompile(`^\d{6}$`)
)

type phoneLoginSession struct {
	created      time.Time
	phone        string
	actionURL    string
	cookies      []*http.Cookie
	userAgent    string
	codeVerifier string
}

// loginEndpoints 按 realm 返回设备授权三端点（auth/state、token、account）+ Origin。
// realm=="global" → 国际版（workbuddy.ai 同域）；cn/非法/缺省 → CN（零回归）。
func loginEndpoints(realm string) (state, token, account, origin string) {
	if realm == "global" {
		base := upstreamBaseGlobal
		return base + "/v2/plugin/auth/state?platform=CLI",
			base + "/v2/plugin/auth/token?state=",
			base + "/v2/plugin/login/account?state=",
			originRefererGlobal
	}
	base := upstreamBaseCN
	return base + "/v2/plugin/auth/state?platform=CLI",
		base + "/v2/plugin/auth/token?state=",
		base + "/v2/plugin/login/account?state=",
		originRefererCN
}

// loginHTTP 设备授权专用 client：短超时、无 cookie（每请求携带 state，无会话态）。
var loginHTTP = &http.Client{Timeout: 30 * time.Second}

func commonHeaders(req *http.Request, origin string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
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

// doJSON 发一次 JSON 请求并解信封。origin 为 Origin/Referer 基础域（随 realm 切）。
func doJSON(method, fullURL, bearer string, body io.Reader, origin string) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	commonHeaders(req, origin)
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
// body 可带 {"realm":"global"}（缺省 cn）；state 会话记 realm，poll 同 realm 落盘。
func (p *Panel) loginStart(w http.ResponseWriter, r *http.Request) {
	realm := "cn"
	if r.Body != nil {
		var reqBody struct {
			Realm string `json:"realm"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&reqBody); err == nil {
			if reqBody.Realm == "global" {
				realm = "global"
			}
		}
	}
	epState, _, _, origin := loginEndpoints(realm)
	data, status, err := doJSON(http.MethodPost, epState, "", bytes.NewReader([]byte("{}")), origin)
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
	for s, sess := range p.logins {
		if time.Since(sess.created) > loginTTL {
			delete(p.logins, s)
		}
	}
	p.logins[st.State] = loginSession{created: time.Now(), realm: realm}
	p.loginMu.Unlock()
	log.Printf("panel: 发起 OAuth 添加账号 realm=%s（state=%s...）", realm, st.State[:min(8, len(st.State))])
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "url": st.AuthURL, "state": st.State, "realm": realm})
}

// loginPoll 轮询登录态。未完成 → {done:false}；完成 → 建凭证、落盘、热加载、签到。
func (p *Panel) loginPoll(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state == "" {
		writeErr(w, http.StatusBadRequest, "missing state")
		return
	}
	p.loginMu.Lock()
	sess, known := p.logins[state]
	p.loginMu.Unlock()
	if !known {
		writeErr(w, http.StatusNotFound, "unknown or expired state（请重新发起添加账号）")
		return
	}
	_, epToken, epAcct, origin := loginEndpoints(sess.realm)

	// auth/token 是权威登录状态端点：pending 时业务 code 非 0（"login ing"）。
	tokRaw, _, err := doJSON(http.MethodGet, epToken+state, "", nil, origin)
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
	if acctRaw, _, err := doJSON(http.MethodGet, epAcct+state, tok.AccessToken, nil, origin); err == nil {
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
	// global 登录：落盘 auth.realm=global（Realm() 按此判域；不写则依赖 domain 后缀回落）。
	if sess.realm == "global" {
		if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
			writeErr(w, http.StatusInternalServerError, "set realm: "+err.Error())
			return
		}
	} else {
		// CN 也显式补 realm 键（幂等），让 auth 文件形态统一（与 LoadDir 存量迁移对齐）。
		_, _ = a.BackfillRealm()
	}
	if err := a.SaveAtomic(); err != nil {
		writeErr(w, http.StatusInternalServerError, "save auth: "+err.Error())
		return
	}
	p.cfg.Pool.Add(a)
	p.cfg.Pool.Revive(acct.UID) // 全新登录 = 人工恢复口径：清掉旧号遗留的禁用/冷却/熔断

	// 顺带签到 + 余额刷新（幂等；失败不影响登录结果，只体现在返回字段里）。
	// realm 分支：CN 走 DailyCheckin；global 无 CN 签到体系，改为注册激活 + trial 领取
	// （D4 门控同 scheduler：CN 任务端点对 global 不发起任何调用）。
	checkinMsg := ""
	remain := int64(-1)
	total := int64(0)
	if sess.realm == "global" {
		// 注册激活（幂等）：region required 时自动补地区（白名单首个，HK）后重新激活。
		// 失败不阻断登录结果（auth 已落盘），只在返回字段里体现。
		if activated, err := p.cfg.Upstream.GlobalCompleteRegistration(a); err != nil {
			checkinMsg = "注册激活失败: " + err.Error()
			log.Printf("panel: global 注册激活 uid=%s: %v", acct.UID, err)
		} else if activated {
			log.Printf("panel: global 注册激活 uid=%s 完成", acct.UID)
		}
		// trial 加油包（幂等 14051 = 已领过，非错误）。
		if claimed, err := p.cfg.Upstream.ClaimTrial(a); err != nil {
			checkinMsg = joinMsg(checkinMsg, "trial 领取失败: "+err.Error())
			log.Printf("panel: global trial uid=%s: %v", acct.UID, err)
		} else if claimed {
			log.Printf("panel: global trial uid=%s 已领", acct.UID)
		}
	} else {
		if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
			checkinMsg = err.Error()
		}
	}
	if rm, tt, err := p.cfg.Upstream.UserResource(a); err == nil {
		remain, total = rm, tt
		p.cfg.Pool.ReenableIfCredits(acct.UID, rm, tt)
	}

	p.loginMu.Lock()
	delete(p.logins, state)
	p.loginMu.Unlock()
	log.Printf("panel: 新账号已热加载 uid=%s nickname=%q realm=%s（免重启生效）", acct.UID, acct.Nickname, sess.realm)
	writeJSON(w, http.StatusOK, map[string]any{
		"done":            true,
		"uid":             acct.UID,
		"nickname":        acct.Nickname,
		"realm":           sess.realm,
		"credits":         remain,
		"credits_total":   total,
		"checkin_message": checkinMsg,
	})
}

// joinMsg 拼接 login 完成后的提示消息（多段用「；」连接，空段跳过）。
func joinMsg(parts ...string) string {
	out := ""
	for _, s := range parts {
		if s == "" {
			continue
		}
		if out != "" {
			out += "；"
		}
		out += s
	}
	return out
}

// loginRegions 返回 global 注册可选地区（panel 前端选地区弹窗用；CN 不调用）。
// 未持账号时返回白名单静态兜底（前端只读展示，不依赖上游）。
func (p *Panel) loginRegions(w http.ResponseWriter, r *http.Request) {
	// 静态白名单（对齐国际版 web 展示集）：面板前端只读展示，无需账号态。
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"regions": []map[string]string{
			{"code": "HK", "name": "Hong Kong"},
			{"code": "MO", "name": "Macao"},
			{"code": "SG", "name": "Singapore"},
			{"code": "TH", "name": "Thailand"},
			{"code": "PH", "name": "Philippines"},
			{"code": "MY", "name": "Malaysia"},
			{"code": "ID", "name": "Indonesia"},
		},
	})
}

// phoneSendCode 创建 Keycloak 手机号登录会话并发送短信验证码。
func (p *Panel) phoneSendCode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone string `json:"phone"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	phone := strings.TrimSpace(body.Phone)
	if !validPhone(phone) {
		writeErr(w, http.StatusBadRequest, "手机号格式错误，请输入11位手机号")
		return
	}

	verifier, challenge, err := phonePKCE()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成登录会话失败: "+err.Error())
		return
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "初始化登录会话失败: "+err.Error())
		return
	}
	client := &http.Client{Timeout: 30 * time.Second, Jar: jar}

	authURL, _ := url.Parse(phoneLoginBase + "/auth/realms/copilot/protocol/openid-connect/auth")
	q := authURL.Query()
	q.Set("client_id", "console")
	q.Set("redirect_uri", phoneLoginBase)
	q.Set("response_type", "code")
	q.Set("scope", "openid profile offline_access email")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	authURL.RawQuery = q.Encode()
	page, status, err := phoneRequest(client, http.MethodGet, authURL.String(), nil, "text/html,application/xhtml+xml")
	if err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("获取登录页面失败（上游 %d）: %v", status, err))
		return
	}
	actionMatch := phoneActionRE.FindSubmatch(page)
	if len(actionMatch) != 2 {
		writeErr(w, http.StatusBadGateway, "登录页面缺少表单 action")
		return
	}
	actionURL := strings.ReplaceAll(string(actionMatch[1]), "&amp;", "&")
	action, err := url.Parse(actionURL)
	if err != nil || action.Query().Get("session_code") == "" {
		writeErr(w, http.StatusBadGateway, "登录页面缺少 session_code")
		return
	}

	smsURL := phoneLoginBase + "/auth/realms/copilot/sms/authentication-code?phoneNumber=" + url.QueryEscape("+86"+phone)
	smsRaw, status, err := phoneRequest(client, http.MethodGet, smsURL, nil, "application/json")
	if err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("发送验证码失败（上游 %d）: %v", status, err))
		return
	}
	// 短信验证码通常 60 秒失效；会话本身仍保留 5 分钟用于返回明确的过期错误。
	expiresIn := 60
	var sms struct {
		ExpiresIn int `json:"expires_in"`
	}
	if json.Unmarshal(smsRaw, &sms) == nil && sms.ExpiresIn > 0 {
		expiresIn = sms.ExpiresIn
	}
	cookies := jar.Cookies(authURL)
	sessionID, err := randomHex(16)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成登录会话失败: "+err.Error())
		return
	}
	p.phoneMu.Lock()
	p.cleanupPhoneLoginsLocked(time.Now())
	p.phoneLogins[sessionID] = phoneLoginSession{
		created: time.Now(), phone: phone, actionURL: actionURL,
		cookies: cookies, userAgent: phoneLoginUA, codeVerifier: verifier,
	}
	p.phoneMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "session_id": sessionID, "expires_in": expiresIn})
}

// phoneLogin 完成验证码登录并复用现有账号落盘/热加载逻辑。
func (p *Panel) phoneLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone     string `json:"phone"`
		Code      string `json:"code"`
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	phone, code, sessionID := strings.TrimSpace(body.Phone), strings.TrimSpace(body.Code), strings.TrimSpace(body.SessionID)
	if !validPhone(phone) || !phoneCodeRE.MatchString(code) || sessionID == "" {
		writeErr(w, http.StatusBadRequest, "手机号、验证码或登录会话无效")
		return
	}
	p.phoneMu.Lock()
	sess, ok := p.phoneLogins[sessionID]
	if ok {
		delete(p.phoneLogins, sessionID) // 验证码会话单次消费，防止重放
	}
	p.phoneMu.Unlock()
	if !ok || time.Since(sess.created) > phoneLoginTTL || sess.phone != phone {
		writeErr(w, http.StatusBadRequest, "登录会话不存在、已过期或手机号不匹配")
		return
	}

	tok, err := phoneCompleteLogin(sess, code)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	acct, err := phoneAccountFromToken(tok.AccessToken, phone)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	acct, err = p.savePhoneAccount(sess, tok, acct)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"done": true, "uid": acct.UID, "nickname": acct.Nickname,
		"credits": acct.Credits, "credits_total": acct.CreditsTotal,
		"checkin_message": acct.CheckinMessage,
	})
}

func validPhone(phone string) bool {
	return phoneRE.MatchString(phone)
}

func phonePKCE() (verifier, challenge string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(hash[:]), nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b), nil
}

func (p *Panel) cleanupPhoneLoginsLocked(now time.Time) {
	for id, sess := range p.phoneLogins {
		if now.Sub(sess.created) > phoneLoginTTL {
			delete(p.phoneLogins, id)
		}
	}
}

func phoneRequest(client *http.Client, method, target string, form url.Values, accept string) ([]byte, int, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, target, body)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", phoneLoginUA)
	req.Header.Set("Accept", accept)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if readErr != nil {
		return nil, resp.StatusCode, readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return raw, resp.StatusCode, fmt.Errorf("上游返回 HTTP %d", resp.StatusCode)
	}
	return raw, resp.StatusCode, nil
}

type phoneToken struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64
	Domain       string
}

func phoneCompleteLogin(sess phoneLoginSession, code string) (phoneToken, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return phoneToken{}, err
	}
	client := &http.Client{Timeout: 30 * time.Second, Jar: jar}
	base, _ := url.Parse(phoneLoginBase)
	jar.SetCookies(base, sess.cookies)
	form := url.Values{
		"phoneNumber":    {"+86" + sess.phone},
		"code":           {code},
		"phoneActivated": {"true"},
		"credentialId":   {""},
		"login":          {"登录"},
	}
	req, err := http.NewRequest(http.MethodPost, sess.actionURL, strings.NewReader(form.Encode()))
	if err != nil {
		return phoneToken{}, err
	}
	req.Header.Set("User-Agent", sess.userAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := client.Do(req)
	if err != nil {
		return phoneToken{}, fmt.Errorf("提交验证码失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if !strings.Contains(resp.Request.URL.String(), "code=") {
		msg := "登录失败"
		if m := regexp.MustCompile(`(?i)kc-feedback[^>]*>([^<]+)`).FindSubmatch(raw); len(m) == 2 {
			msg = strings.TrimSpace(string(m[1]))
		}
		return phoneToken{}, errors.New(msg)
	}
	callback := resp.Request.URL.Query().Get("code")
	if callback == "" {
		return phoneToken{}, errors.New("登录回调缺少授权码")
	}
	tokenURL := phoneLoginBase + "/auth/realms/copilot/protocol/openid-connect/token"
	redirect := phoneLoginBase
	for _, attempt := range []struct {
		clientID string
		verifier string
	}{
		{"console", sess.codeVerifier},
		{"console", ""},
	} {
		form := url.Values{
			"grant_type":   {"authorization_code"},
			"client_id":    {attempt.clientID},
			"code":         {callback},
			"redirect_uri": {redirect},
		}
		if attempt.verifier != "" {
			form.Set("code_verifier", attempt.verifier)
		}
		raw, _, err := phoneRequest(client, http.MethodPost, tokenURL, form, "application/json")
		if err == nil {
			var v struct {
				AccessToken string `json:"access_token"`
				Refresh     string `json:"refresh_token"`
				ExpiresIn   int64  `json:"expires_in"`
				Domain      string `json:"domain"`
			}
			if json.Unmarshal(raw, &v) == nil && v.AccessToken != "" {
				if v.ExpiresIn <= 0 {
					v.ExpiresIn = 3600
				}
				return phoneToken{AccessToken: v.AccessToken, RefreshToken: v.Refresh, ExpiresIn: v.ExpiresIn, Domain: v.Domain}, nil
			}
		}
	}
	// console 客户端可能要求未公开的 client_secret；复用已登录 Cookie，
	// 用 account 公共客户端重新取得授权码再交换 Token。
	if acctToken, ok := phoneAccountTokenFallback(client, tokenURL); ok {
		return acctToken, nil
	}
	return phoneToken{}, errors.New("登录成功但 Token 交换失败，请重新发送验证码")
}

func phoneAccountTokenFallback(client *http.Client, tokenURL string) (phoneToken, bool) {
	verifier, challenge, err := phonePKCE()
	if err != nil {
		return phoneToken{}, false
	}
	redirect := phoneLoginBase + "/auth/realms/copilot/account/"
	authURL, _ := url.Parse(phoneLoginBase + "/auth/realms/copilot/protocol/openid-connect/auth")
	q := authURL.Query()
	q.Set("client_id", "account")
	q.Set("redirect_uri", redirect)
	q.Set("response_type", "code")
	q.Set("scope", "openid profile offline_access email")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	authURL.RawQuery = q.Encode()
	accountClient := *client
	accountClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequest(http.MethodGet, authURL.String(), nil)
	if err != nil {
		return phoneToken{}, false
	}
	req.Header.Set("User-Agent", phoneLoginUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := accountClient.Do(req)
	if err != nil {
		return phoneToken{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusSeeOther {
		return phoneToken{}, false
	}
	codeURL, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		return phoneToken{}, false
	}
	code := codeURL.Query().Get("code")
	if code == "" {
		return phoneToken{}, false
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {"account"},
		"code":          {code},
		"redirect_uri":  {redirect},
		"code_verifier": {verifier},
	}
	raw, _, err := phoneRequest(client, http.MethodPost, tokenURL, form, "application/json")
	if err != nil {
		return phoneToken{}, false
	}
	var v struct {
		AccessToken string `json:"access_token"`
		Refresh     string `json:"refresh_token"`
		ExpiresIn   int64  `json:"expires_in"`
		Domain      string `json:"domain"`
	}
	if json.Unmarshal(raw, &v) != nil || v.AccessToken == "" {
		return phoneToken{}, false
	}
	if v.ExpiresIn <= 0 {
		v.ExpiresIn = 3600
	}
	return phoneToken{AccessToken: v.AccessToken, RefreshToken: v.Refresh, ExpiresIn: v.ExpiresIn, Domain: v.Domain}, true
}

type phoneAccount struct {
	UID            string
	EnterpriseID   string
	Nickname       string
	Domain         string
	Credits        int64
	CreditsTotal   int64
	CheckinMessage string
}

func phoneAccountFromToken(accessToken, phone string) (phoneAccount, error) {
	parts := strings.Split(accessToken, ".")
	if len(parts) < 2 {
		return phoneAccount{}, errors.New("登录成功但 Token 不是可解析的 JWT，无法取得账号 UID")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return phoneAccount{}, errors.New("登录成功但 Token claims 无法解析")
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return phoneAccount{}, errors.New("登录成功但 Token claims 格式错误")
	}
	get := func(keys ...string) string {
		for _, key := range keys {
			if v, ok := claims[key].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
	uid := get("uid", "userId", "user_id", "sub")
	if !validUID(uid) {
		return phoneAccount{}, errors.New("登录成功但 Token 中缺少有效 UID，拒绝写入账号")
	}
	nickname := get("nickname", "preferred_username", "name", "username")
	if nickname == "" {
		nickname = maskPhone(phone)
	}
	domain := get("domain", "iss")
	if strings.HasPrefix(domain, "https://") {
		if parsed, err := url.Parse(domain); err == nil {
			domain = parsed.Host
		}
	}
	if domain == "" {
		domain = "www.codebuddy.cn"
	}
	return phoneAccount{UID: uid, EnterpriseID: get("enterpriseId", "enterprise_id"), Nickname: nickname, Domain: domain}, nil
}

func maskPhone(phone string) string {
	if len(phone) < 11 {
		return phone
	}
	return phone[:3] + "****" + phone[7:]
}

func (p *Panel) savePhoneAccount(sess phoneLoginSession, tok phoneToken, acct phoneAccount) (phoneAccount, error) {
	if err := os.MkdirAll(p.cfg.AuthDir, 0o755); err != nil {
		return acct, fmt.Errorf("mkdir auth dir: %w", err)
	}
	a := &auth.Auth{
		AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken,
		ExpiresAt: time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
		Domain:    tok.Domain, UID: acct.UID, EnterpriseID: acct.EnterpriseID,
		Nickname: acct.Nickname, FilePath: filepath.Join(p.cfg.AuthDir, "workbuddy-"+acct.UID+".json"),
	}
	if a.Domain == "" {
		a.Domain = acct.Domain
	}
	if _, err := auth.BackfillRealmFor(a, "cn"); err != nil {
		return acct, fmt.Errorf("set realm: %w", err)
	}
	if err := a.SaveAtomic(); err != nil {
		return acct, fmt.Errorf("save auth: %w", err)
	}
	p.cfg.Pool.Add(a)
	p.cfg.Pool.Revive(acct.UID)
	if p.cfg.Upstream != nil {
		if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
			acct.CheckinMessage = err.Error()
		}
		if rm, tt, err := p.cfg.Upstream.UserResource(a); err == nil {
			acct.Credits, acct.CreditsTotal = rm, tt
			p.cfg.Pool.ReenableIfCredits(acct.UID, rm, tt)
		}
	}
	log.Printf("panel: 手机号登录新账号已热加载 uid=%s nickname=%q phone=%s", acct.UID, acct.Nickname, maskPhone(sess.phone))
	return acct, nil
}
