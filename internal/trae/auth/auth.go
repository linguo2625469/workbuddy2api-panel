// Package auth 解析 TRAE SOLO auth 文件（嵌套形：auth + account），
// 提供原子写回与目录扫描。
package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Auth 归一化后的账号凭证。
//
// 并发模型：mu 保护可变字段（AccessToken/RefreshToken/ExpiresAt），
// 写路径（upstream.RefreshToken）持写锁整段执行 ExchangeToken；
// 读路径（NeedsRefresh/JWT/RefreshTokenValue）持读锁读取快照，
// 杜绝与写并发时的数据竞争（-race 实测确认）。
// 其余字段（Domain/ApiHost/MachineID/DeviceID/UID/...）加载后不变，直接读。
type Auth struct {
	mu sync.RWMutex

	AccessToken  string // Cloud-IDE-JWT 头用
	RefreshToken string // 每次 ExchangeToken 轮换
	ExpiresAt    int64  // Unix 秒（accessToken 过期时刻）
	Domain       string // "trae.cn"
	ApiHost      string // "https://api.trae.com.cn"（ExchangeToken host）
	MachineID    string // x-machine-id
	DeviceID     string // x-device-id
	UID          string
	EnterpriseID string
	Nickname     string
	FilePath     string // 落盘路径；refresh 后原子写回
}

// Lock 供同进程内其他包（upstream.RefreshToken）在改写 Auth 字段期间加写锁。
func (a *Auth) Lock() { a.mu.Lock() }

// Unlock 释放 a.Lock 获取的写锁。
func (a *Auth) Unlock() { a.mu.Unlock() }

// RLock 供读路径持有读锁（与写锁互斥，读读不互斥）。
func (a *Auth) RLock() { a.mu.RLock() }

// RUnlock 释放 a.RLock 获取的读锁。
func (a *Auth) RUnlock() { a.mu.RUnlock() }

// JWT 返回当前 accessToken 的读锁快照，防与 RefreshToken 写并发竞态。
func (a *Auth) JWT() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.AccessToken
}

// RefreshTokenValue 返回当前 refreshToken 的读锁快照，防与 RefreshToken 写并发竞态。
func (a *Auth) RefreshTokenValue() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.RefreshToken
}

// NeedsRefresh 报告 token 是否将在 within 内过期（或已过期/无 expiry）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.NeedsRefreshLocked(within)
}

// NeedsRefreshLocked 是 NeedsRefresh 的持锁内部版本；调用方必须已持有 a.mu（读或写锁）。
func (a *Auth) NeedsRefreshLocked(within time.Duration) bool {
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// parseNested 兼容现有 trae-*.json 嵌套形：
//
//	{"account":{...},"auth":{...}}
func parseNested(raw []byte) (*Auth, error) {
	var n struct {
		Auth struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			Domain       string `json:"domain"`
			ApiHost      string `json:"apiHost"`
			MachineID    string `json:"machineId"`
			DeviceID     string `json:"deviceId"`
		} `json:"auth"`
		Account struct {
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
		} `json:"account"`
	}
	if err := json.Unmarshal(raw, &n); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	return &Auth{
		AccessToken:  n.Auth.AccessToken,
		RefreshToken: n.Auth.RefreshToken,
		ExpiresAt:    n.Auth.ExpiresAt,
		Domain:       n.Auth.Domain,
		ApiHost:      n.Auth.ApiHost,
		MachineID:    n.Auth.MachineID,
		DeviceID:     n.Auth.DeviceID,
		UID:          n.Account.UID,
		EnterpriseID: n.Account.EnterpriseID,
		Nickname:     n.Account.Nickname,
	}, nil
}

// parseFlat 兼容扁平形（CPA 面板手建等）：
//
//	{"accessToken":...,"uid":...,"machineId":...,"deviceId":...}
func parseFlat(raw []byte) (*Auth, error) {
	var f struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"`
		Domain       string `json:"domain"`
		ApiHost      string `json:"apiHost"`
		MachineID    string `json:"machineId"`
		DeviceID     string `json:"deviceId"`
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	return &Auth{
		AccessToken:  f.AccessToken,
		RefreshToken: f.RefreshToken,
		ExpiresAt:    f.ExpiresAt,
		Domain:       f.Domain,
		ApiHost:      f.ApiHost,
		MachineID:    f.MachineID,
		DeviceID:     f.DeviceID,
		UID:          f.UID,
		EnterpriseID: f.EnterpriseID,
		Nickname:     f.Nickname,
	}, nil
}

// Parse 兼容两种磁盘形态：
//
//	嵌套形 {"auth":{...},"account":{...}}  （登录脚本产出，现有 trae-*.json）
//	扁平形 {"accessToken":...,"uid":...}   （手建）
func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	var (
		a   *Auth
		err error
	)
	if _, nested := probe["auth"]; nested {
		a, err = parseNested(raw)
	} else {
		a, err = parseFlat(raw)
	}
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	return a, nil
}

// SaveAtomic 以嵌套形原子写回 FilePath（tmp + rename，0600），保持登录脚本可读格式。
// 加锁外壳：防止与 RefreshToken 并发读写 token 字段导致写回半更新。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.saveAtomicLocked()
}

// saveAtomicLocked 是 SaveAtomic 的持锁内部版本；调用方必须已持有 a.mu。
func (a *Auth) saveAtomicLocked() error {
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  a.AccessToken,
			"refreshToken": a.RefreshToken,
			"expiresAt":    a.ExpiresAt,
			"domain":       a.Domain,
			"apiHost":      a.ApiHost,
			"machineId":    a.MachineID,
			"deviceId":     a.DeviceID,
		},
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			"nickname":     a.Nickname,
		},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.FilePath)
}

// FilePathFor 在已知 AuthDir 时构造 trae-{uid}.json 落盘路径。
func FilePathFor(authDir, uid string) string {
	return filepath.Join(authDir, "trae-"+uid+".json")
}

// MaskToken 保留前 n 字符 + 省略号；不足则全显示。用于面板 JSON 预览脱敏。
func MaskToken(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(" + strconv.Itoa(len(s)) + " chars)"
}

// LoadDir 扫描 dir 下 trae-*.json。解析失败的文件静默跳过（启动日志由调用方统计）。
func LoadDir(dir string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, "trae-*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = f
		out = append(out, a)
	}
	return out, nil
}
