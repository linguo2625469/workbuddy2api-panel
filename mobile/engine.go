// Package mobile 是 Android 签到 App 的 gomobile 绑定入口。
// 只暴露 string/error，数据以 JSON 字符串交换；签到逻辑复用 internal/auth 与
// internal/upstream，不在 Kotlin 侧重写协议。
package mobile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const (
	requestTimeout = 35 * time.Second
	dnsPrimary     = "223.5.5.5"
	dnsSecondary   = "119.29.29.29"
	refreshWindow  = 2 * time.Hour
)

// Engine 持有账号目录与上游客户端；每个方法都可从任意线程调用，
// 内部用互斥锁串行化操作，避免并发签到互相覆盖已轮换的 token。
type Engine struct {
	mu  sync.Mutex
	dir string
	up  *upstream.Client

	// testHook 仅供测试观察串行化；生产恒为 nil。
	testHook func()
}

// NewEngine 创建引擎；dir 为 App 私有账号目录。
// 显式 DNS：Android 没有 /etc/resolv.conf，Go 解析器会退回 [::1]:53，
// 传入公共 DNS 绕开（见 internal/upstream/dns.go 注释）。
func NewEngine(dir string) *Engine {
	up := upstream.New()
	up.HTTP.Timeout = requestTimeout
	up.SetDNSDialer(upstream.NewDNSDialer([]string{dnsPrimary, dnsSecondary}))
	return &Engine{dir: dir, up: up}
}

// newEngineWith 仅供测试注入替换后的上游客户端。
func newEngineWith(dir string, up *upstream.Client) *Engine {
	return &Engine{dir: dir, up: up}
}

type accountJSON struct {
	UID        string `json:"uid"`
	Nickname   string `json:"nickname"`
	Realm      string `json:"realm"`
	Enterprise bool   `json:"enterprise"`
	File       string `json:"file"`
}

type checkinResult struct {
	UID       string `json:"uid"`
	Nickname  string `json:"nickname"`
	Status    string `json:"status"`
	Message   string `json:"message"`
	CheckedAt string `json:"checked_at"`
}

// ImportAccount 解析并保存单个账号文件内容；同 UID 覆盖旧文件。
func (e *Engine) ImportAccount(content string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	a, err := auth.Parse([]byte(content))
	if err != nil {
		return "", fmt.Errorf("账号文件解析失败: %w", err)
	}
	a.UID = strings.TrimSpace(a.UID)
	if a.UID == "" {
		return "", fmt.Errorf("缺少 uid，无法导入")
	}
	if !validUID(a.UID) {
		return "", fmt.Errorf("uid 含非法字符，无法导入")
	}
	if err := os.MkdirAll(e.dir, 0o700); err != nil {
		return "", fmt.Errorf("创建账号目录失败: %w", err)
	}
	a.FilePath = filepath.Join(e.dir, "workbuddy-"+a.UID+".json")
	if a.RealmStored() == "" {
		a.BackfillRealm()
	}
	if err := a.SaveAtomic(); err != nil {
		return "", fmt.Errorf("账号文件写入失败: %w", err)
	}
	return marshal(accountOf(a))
}

// ListAccounts 返回全部账号的 JSON 数组，按文件名排序。
func (e *Engine) ListAccounts() (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	list, err := auth.LoadDir(e.dir)
	if err != nil {
		return "", fmt.Errorf("读取账号目录失败: %w", err)
	}
	out := make([]accountJSON, 0, len(list))
	for _, a := range list {
		out = append(out, accountOf(a))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return marshal(out)
}

func accountOf(a *auth.Auth) accountJSON {
	return accountJSON{
		UID:        a.UID,
		Nickname:   a.Nickname,
		Realm:      a.Realm(),
		Enterprise: a.IsEnterprise(),
		File:       filepath.Base(a.FilePath),
	}
}

// validUID 与面板登录的字符白名单一致：字母/数字/连字符/下划线，长度 <= 64。
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

func marshal(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// Checkin 对单号执行签到：企业号 / 国际版号本地跳过，其余调 DailyCheckin。
func (e *Engine) Checkin(uid string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.testHook != nil {
		e.testHook()
	}

	a, err := e.load(uid)
	if err != nil {
		return "", err
	}
	r := checkinResult{
		UID:       a.UID,
		Nickname:  a.Nickname,
		CheckedAt: time.Now().Format(time.RFC3339),
	}
	switch {
	case a.IsEnterprise():
		r.Status, r.Message = "skipped", "企业账号无签到体系（已跳过）"
	case a.IsGlobal():
		r.Status, r.Message = "skipped", "国际版账号无签到体系（已跳过）"
	default:
		if st, msg, done := e.refreshIfNeeded(a); done {
			r.Status, r.Message = st, msg
		} else if err := e.up.DailyCheckin(a); err != nil {
			if upstream.IsAlreadyCheckin(err) {
				r.Status, r.Message = "already", "今天已签到"
			} else {
				r.Status, r.Message = "failed", short(err.Error())
			}
		} else {
			r.Status, r.Message = "ok", "签到成功"
		}
	}
	return marshal(r)
}

// refreshIfNeeded 在 token 临近过期时刷新并写回；done=true 表示不应继续签到。
func (e *Engine) refreshIfNeeded(a *auth.Auth) (status, message string, done bool) {
	if !a.NeedsRefresh(refreshWindow) {
		return "", "", false
	}
	if a.RefreshTokenValue() == "" {
		return "auth_invalid", "凭证已过期且无 refreshToken，请重新导入该账号文件", true
	}
	if err := e.up.RefreshToken(a); err != nil {
		if isSessionDead(err) {
			return "auth_invalid", "凭证已失效，请重新导入该账号文件", true
		}
		return "failed", "刷新凭证失败：" + short(err.Error()), true
	}
	if err := a.SaveAtomic(); err != nil {
		return "failed", "token 刷新成功但写回失败：" + short(err.Error()), true
	}
	return "", "", false
}

func isSessionDead(err error) bool {
	var ue *upstream.Error
	return errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead
}

func (e *Engine) load(uid string) (*auth.Auth, error) {
	list, err := auth.LoadDir(e.dir)
	if err != nil {
		return nil, err
	}
	for _, a := range list {
		if a.UID == uid {
			return a, nil
		}
	}
	return nil, fmt.Errorf("account not found: %s", uid)
}

// short 把上游错误压成一行，按 rune 截断避免切坏中文。
func short(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > 160 {
		return string(r[:160]) + "…"
	}
	return s
}
