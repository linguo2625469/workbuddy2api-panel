// login.go Web 登录闭环：生成登录 URL → pending 态 → /authorize 回调捕获 →
// ExchangeToken + GetUserInfo + 落盘 → 面板轮询 result。
//
// 回调端口策略（M0 结论：双端口 18080）：
//   - TRAE 登录页回调到 http://127.0.0.1:18080/authorize
//   - main.go 起第二个 http.Server 监听 18080，复用同一 Handler（/authorize 在主 mux 已注册）
//   - 回调捕获后直接在服务端完成导入（用户无需再粘赖）
//
// pending 态只在内存（重启丢失），符合「登录态瞬时」语义。
package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/trae/auth"
)

// pendingState pending 登录状态。
type pendingState string

const (
	pendingStateActive   pendingState = "pending"  // 等待用户登录回调
	pendingStateSuccess  pendingState = "success"  // 回调捕获 + 落盘成功
	pendingStateFailed   pendingState = "failed"   // ExchangeToken/落盘失败
	pendingStateCanceled pendingState = "canceled" // 用户取消
)

// pendingLogin 单次登录的临时上下文。
type pendingLogin struct {
	state       pendingState
	machineID   string
	deviceID    string
	callbackURL string // auth_callback_url（含端口）
	createdAt   time.Time

	// success 时填
	uid      string
	nickname string
	// failed 时填
	errMsg string
}

// loginStartResponse POST /admin/api/login 返回。
type loginStartResponse struct {
	LoginURL    string `json:"login_url"`
	PendingID   string `json:"pending_id"`
	CallbackURL string `json:"callback_url"`
}

// adminLoginStart POST /admin/api/login：生成登录 URL + pending 态。
// body 可选 {callback_port: "18080"}；默认用 127.0.0.1:18080/authorize。
func (h *Handler) adminLoginStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CallbackPort string `json:"callback_port,omitempty"`
	}
	_ = decodeBodyOptional(r, &req)
	if req.CallbackPort == "" {
		req.CallbackPort = "18080"
	}
	callbackURL := "http://127.0.0.1:" + req.CallbackPort + "/authorize"

	machineID, err := randomHex(16) // hex32
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "rand_failed", err.Error())
		return
	}
	deviceID, err := randomHex(16)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "rand_failed", err.Error())
		return
	}
	loginURL := BuildLoginURL(machineID, deviceID, callbackURL)

	pendingID, err := randomHex(8) // hex16
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "rand_failed", err.Error())
		return
	}
	pl := &pendingLogin{
		state:       pendingStateActive,
		machineID:   machineID,
		deviceID:    deviceID,
		callbackURL: callbackURL,
		createdAt:   time.Now(),
	}
	h.loginMu.Lock()
	h.logins[pendingID] = pl
	h.loginMu.Unlock()

	writeJSON(w, http.StatusOK, loginStartResponse{
		LoginURL: loginURL, PendingID: pendingID, CallbackURL: callbackURL,
	})
}

// adminLoginResult GET /admin/api/login/result?pending_id=...
func (h *Handler) adminLoginResult(w http.ResponseWriter, r *http.Request) {
	pendingID := r.URL.Query().Get("pending_id")
	pl, ok := h.getPending(pendingID)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "pending login not found (expired or invalid)")
		return
	}
	resp := map[string]any{
		"pending_id": pendingID,
		"state":      string(pl.state),
	}
	if pl.state == pendingStateSuccess {
		resp["uid"] = pl.uid
		resp["nickname"] = pl.nickname
	} else if pl.state == pendingStateFailed {
		resp["error"] = pl.errMsg
	}
	writeJSON(w, http.StatusOK, resp)
}

// adminLoginCancel POST /admin/api/login/cancel（body {pending_id} 或 query ?pending_id=）
func (h *Handler) adminLoginCancel(w http.ResponseWriter, r *http.Request) {
	pendingID := r.URL.Query().Get("pending_id")
	if pendingID == "" {
		var req struct {
			PendingID string `json:"pending_id"`
		}
		_ = decodeBodyOptional(r, &req)
		pendingID = req.PendingID
	}
	h.loginMu.Lock()
	pl, ok := h.logins[pendingID]
	if ok && pl.state == pendingStateActive {
		pl.state = pendingStateCanceled
	}
	// 直接删 pending（取消后不再保留）
	delete(h.logins, pendingID)
	h.loginMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"pending_id": pendingID, "canceled": ok})
}

// authorizeCallback GET /authorize：TRAE 回调落点。
// 捕获 query → 解析 → ExchangeToken → GetUserInfo → 落盘 → 标记 pending 成功。
// pending_id 通过 TRAE 无法回传（回调 URL 固定），用 machine_id 反查 pending（一一对应）。
func (h *Handler) authorizeCallback(w http.ResponseWriter, r *http.Request) {
	rawURL := r.URL.String()
	if !strings.HasPrefix(rawURL, "http") {
		// /authorize?... → 补全成完整 URL 供 ParseCallback
		rawURL = "http://127.0.0.1" + rawURL
	}
	info, err := ParseCallback(rawURL)
	if err != nil {
		// 回调解析失败：展示一个友好错误页（非 JSON，因为是浏览器跳转）
		authorizeRender(w, http.StatusBadRequest, "登录回调解析失败", err.Error())
		return
	}
	// 用 machine_id 反查 pending（pending 的 machineID 与回调 query.machine_id 一致）
	// 兜底：若 query 无 machine_id 或无 pending，仍可凭 refreshToken 落盘（无 pending 上下文则 machine/device 用回调里的或新生成）
	machineID := r.URL.Query().Get("machine_id")
	deviceID := r.URL.Query().Get("device_id")
	traceID := r.URL.Query().Get("loginTraceID")
	// TRAE 回调不回传 machine_id/device_id，但回传 loginTraceID（= machineTraceID(machine,device) 派生）
	// → 用 loginTraceID 反查 pending 拿回登录时生成的那一对 id，保证凭证与登录态一致
	if machineID == "" || deviceID == "" {
		if traceID != "" {
			if pl, ok := h.getPendingByTrace(traceID); ok {
				machineID, deviceID = pl.machineID, pl.deviceID
			}
		}
	}

	a := &auth.Auth{
		AccessToken:  info.AccessToken,
		RefreshToken: info.RefreshToken,
		UID:          info.UID,
		Nickname:     info.Nickname,
		EnterpriseID: info.EnterpriseID,
		Domain:       "trae.cn",
		ApiHost:      "https://api.trae.com.cn",
		MachineID:    machineID,
		DeviceID:     deviceID,
		ExpiresAt:    info.ExpiresAt,
	}
	// 有 refreshToken → ExchangeToken 换新 access + 轮换 refreshToken
	if a.RefreshToken != "" {
		if exErr := h.cfg.Upstream.RefreshToken(a); exErr != nil {
			// 失败也继续（可能 refreshToken 已被轮换），但标记错误
			h.markPendingByMachine(machineID, traceID, pendingStateFailed, "", "", exErr.Error())
			authorizeRender(w, http.StatusBadGateway, "ExchangeToken 失败", exErr.Error())
			return
		}
	}
	// GetUserInfo 补全 uid/nickname
	uid, nick, ent, gErr := h.cfg.Upstream.GetUserInfo(a)
	if gErr == nil && uid != "" {
		a.UID = uid
		if nick != "" {
			a.Nickname = nick
		}
		if ent != "" {
			a.EnterpriseID = ent
		}
	}
	if a.UID == "" {
		err := errors.New("cannot determine uid from callback or GetUserInfo")
		h.markPendingByMachine(machineID, traceID, pendingStateFailed, "", "", err.Error())
		authorizeRender(w, http.StatusBadRequest, "登录失败", err.Error())
		return
	}
	if a.AccessToken == "" {
		err := errors.New("no access token after exchange")
		h.markPendingByMachine(machineID, traceID, pendingStateFailed, "", "", err.Error())
		authorizeRender(w, http.StatusBadRequest, "登录失败", err.Error())
		return
	}

	// 落盘
	if a.FilePath == "" {
		a.FilePath = auth.FilePathFor(h.cfg.AuthDir, a.UID)
	}
	if mkErr := mkdirAll(h.cfg.AuthDir); mkErr != nil {
		h.markPendingByMachine(machineID, traceID, pendingStateFailed, a.UID, a.Nickname, mkErr.Error())
		authorizeRender(w, http.StatusInternalServerError, "落盘失败", mkErr.Error())
		return
	}
	if err := a.SaveAtomic(); err != nil {
		h.markPendingByMachine(machineID, traceID, pendingStateFailed, a.UID, a.Nickname, err.Error())
		authorizeRender(w, http.StatusInternalServerError, "落盘失败", err.Error())
		return
	}
	h.cfg.Pool.Add(a)
	h.markPendingByMachine(machineID, traceID, pendingStateSuccess, a.UID, a.Nickname, "")

	authorizeRender(w, http.StatusOK, "登录成功", "账号 "+a.UID+"（"+a.Nickname+"）已添加，可关闭此窗口返回面板。")
}

// markPendingByMachine 用 machineID（或 loginTraceID 派生匹配）反查 pending 并标记状态。
func (h *Handler) markPendingByMachine(machineID, traceID string, state pendingState, uid, nick, errMsg string) {
	if machineID == "" && traceID == "" {
		return
	}
	h.loginMu.Lock()
	defer h.loginMu.Unlock()
	for _, pl := range h.logins {
		if pl.machineID == machineID || (traceID != "" && machineTraceID(pl.machineID, pl.deviceID) == traceID) {
			pl.state = state
			if uid != "" {
				pl.uid = uid
			}
			if nick != "" {
				pl.nickname = nick
			}
			if errMsg != "" {
				pl.errMsg = errMsg
			}
			return
		}
	}
}

// getPendingByTrace 用 loginTraceID（= machineTraceID(machine,device)）反查 pending。
// TRAE 回调不回传 machine_id/device_id，但回传 loginTraceID，可据此关联回登录时生成的 id 对。
func (h *Handler) getPendingByTrace(traceID string) (*pendingLogin, bool) {
	if traceID == "" {
		return nil, false
	}
	h.loginMu.Lock()
	defer h.loginMu.Unlock()
	for _, pl := range h.logins {
		if machineTraceID(pl.machineID, pl.deviceID) == traceID {
			return pl, true
		}
	}
	return nil, false
}

// getPending 取 pending（不存在或已过期 false）。pending TTL 10 分钟。
func (h *Handler) getPending(id string) (*pendingLogin, bool) {
	if id == "" {
		return nil, false
	}
	h.loginMu.Lock()
	defer h.loginMu.Unlock()
	pl, ok := h.logins[id]
	if !ok {
		return nil, false
	}
	if time.Since(pl.createdAt) > 10*time.Minute {
		delete(h.logins, id)
		return nil, false
	}
	return pl, true
}

// randomHex 生成 n 字节随机 hex（2n hex 字符）。
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
