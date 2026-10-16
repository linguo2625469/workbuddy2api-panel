// apikey.go 面板「添加 API Key 账号」：CodeBuddy 平台签发的静态 API Key
// （ck_/pt_ 前缀）直接可用，无需 OAuth 设备流。
//
// 落盘形态与 OAuth 账号完全一致（auths/workbuddy-<uid>.json，嵌套形，0600），
// 区别仅在没有 refreshToken / expiresAt 与 enterpriseId——静态凭证不可刷新，
// 额度走企业口径端点（见 upstream.UserResourceDetailedWithExpiry 的分流）。
package panel

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// apiKeyUIDMaxLen 派生/显式 uid 的长度上限（与 validUID 同口径：拼文件名的兜底）。
const apiKeyUIDMaxLen = 64

// apiKeyPrefixes 静态 API Key 前缀集合，用于派生 uid 时剥前缀。
// 前缀合法性判定走 auth.IsAPIKey()（单一判据来源），此处仅作字符串裁剪。
var apiKeyPrefixes = []string{"ck_", "pt_"}

// addAPIKey 添加一个静态 API Key 账号。
//
//	POST /panel/api/accounts/add_apikey
//	body: {"access_token":"ck_...","nickname":"","uid":""}
//
// 流程：校验前缀 → 解析/派生 uid → 用企业额度端点探测 key 是否有效（无效即 400，
// 不落盘）→ 落盘 → 热加载进池并写入探测到的额度。
func (p *Panel) addAPIKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AccessToken string `json:"access_token"`
		Nickname    string `json:"nickname"`
		UID         string `json:"uid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	key := strings.TrimSpace(body.AccessToken)
	// 前缀判据复用 auth：构造一个只带 token 的临时 Auth 走 IsAPIKey（避免前缀
	// 集合在 panel 与 auth 两处各写一遍）。
	if !(&auth.Auth{AccessToken: key}).IsAPIKey() {
		writeErr(w, http.StatusBadRequest, "access_token 必须是 ck_/pt_ 前缀的 API Key")
		return
	}
	uid, err := resolveAPIKeyUID(body.UID, key)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// 探测 key 是否有效：静态 Key 的额度只能走企业口径端点（个人 get-user-resource
	// 对它返回空 Accounts）。探测失败（无效 Key / 上游报错）即 400，不落盘。
	probe := &auth.Auth{AccessToken: key}
	remain, total, expiring, earliestAt, earliestRemaining, err := p.cfg.Upstream.UserResourceDetailedWithExpiry(probe, p.expiringSoonWindow())
	if err != nil {
		writeErr(w, http.StatusBadRequest, "API Key 无效或额度查询失败："+err.Error())
		return
	}

	a := &auth.Auth{
		AccessToken: key,
		// RefreshToken 留空：静态 Key 无法 refresh（refresh 端点只认 OAuth 设备流）；
		// ExpiresAt 留 0（静态凭证无过期），EnterpriseID 留空（无企业身份，也不注入
		// X-Enterprise-Id / X-Tenant-Id）。
		UID:      uid,
		Nickname: strings.TrimSpace(body.Nickname),
		FilePath: filepath.Join(p.cfg.AuthDir, fmt.Sprintf("workbuddy-%s.json", uid)),
	}
	// realm 按现有约定补全（静态 Key 恒 CN；与 LoadDir 存量迁移同口径）。
	_, _ = a.BackfillRealm()
	if err := os.MkdirAll(p.cfg.AuthDir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "mkdir auth dir: "+err.Error())
		return
	}
	if err := a.SaveAtomic(); err != nil {
		writeErr(w, http.StatusInternalServerError, "save auth: "+err.Error())
		return
	}
	p.cfg.Pool.Add(a) // upsert：已存在则覆盖凭证、保留原池状态
	p.cfg.Pool.SetCreditsDetailed(uid, remain, total, expiring, earliestAt, earliestRemaining)
	log.Printf("panel: 添加 API Key 账号 uid=%s nickname=%q credits=%d/%d", uid, a.Nickname, remain, total)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "nickname": a.Nickname, "api_key": true})
}

// resolveAPIKeyUID 解析落盘用 uid：显式传入时按字符白名单校验（拒绝路径穿越）；
// 缺省时取 key 的 <id> 段（ck_<id>.<secret> 的 id）拼成 apikey-<id>，非法字符剔除。
func resolveAPIKeyUID(explicit, key string) (string, error) {
	if e := strings.TrimSpace(explicit); e != "" {
		if !validImportUID(e) {
			return "", fmt.Errorf("uid 只能包含字母、数字、下划线、连字符（≤%d 字符）", apiKeyUIDMaxLen)
		}
		return e, nil
	}
	id := apiKeyID(key)
	if id == "" {
		return "", fmt.Errorf("无法从 key 解析 id 段，请显式指定 uid")
	}
	uid := "apikey-" + id
	if len(uid) > apiKeyUIDMaxLen {
		uid = uid[:apiKeyUIDMaxLen]
	}
	return uid, nil
}

// apiKeyID 取 ck_<id>.<secret> 的 <id> 段（剥前缀、截到首个点之前），只保留
// [A-Za-z0-9_-]——拼文件名前必须净化，避免路径穿越。
func apiKeyID(key string) string {
	s := key
	for _, p := range apiKeyPrefixes {
		if strings.HasPrefix(s, p) {
			s = s[len(p):]
			break
		}
	}
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[:i]
	}
	var b strings.Builder
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			b.WriteRune(c)
		}
	}
	return b.String()
}
