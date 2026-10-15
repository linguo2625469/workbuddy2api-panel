package auth

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// 现有 trae-*.json 的真实结构（敏感字段已脱敏为占位）。
const existingFormat = `{
  "account": {"uid": "1000000000000000", "enterpriseId": "test-ent-id", "nickname": "测试用户"},
  "auth": {
    "accessToken": "at-placeholder", "refreshToken": "rt-placeholder", "expiresAt": 1786805537,
    "domain": "trae.cn", "apiHost": "https://api.trae.com.cn",
    "machineId": "abcdef0123456789abcdef0123456789", "deviceId": "0123456789abcdef0123456789abcdef"
  }
}`

func TestParseExistingFormat(t *testing.T) {
	a, err := Parse([]byte(existingFormat))
	if err != nil {
		t.Fatalf("parse existing format: %v", err)
	}
	if a.UID != "1000000000000000" || a.EnterpriseID != "test-ent-id" || a.Nickname != "测试用户" {
		t.Errorf("account: %+v", a)
	}
	if a.AccessToken != "at-placeholder" || a.RefreshToken != "rt-placeholder" || a.ExpiresAt != 1786805537 {
		t.Errorf("tokens: %+v", a)
	}
	if a.Domain != "trae.cn" || a.ApiHost != "https://api.trae.com.cn" {
		t.Errorf("hosts: %+v", a)
	}
	if len(a.MachineID) != 32 || len(a.DeviceID) != 32 {
		t.Errorf("ids: machine=%q device=%q", a.MachineID, a.DeviceID)
	}
}

func TestParseFlat(t *testing.T) {
	raw := []byte(`{"accessToken":"at","refreshToken":"rt","expiresAt":1753600000,"uid":"u2","nickname":"n2","machineId":"m1","deviceId":"d1"}`)
	a, err := Parse(raw)
	if err != nil || a.UID != "u2" || a.AccessToken != "at" || a.MachineID != "m1" || a.DeviceID != "d1" {
		t.Fatalf("flat: %+v %v", a, err)
	}
}

func TestParseMissingToken(t *testing.T) {
	if _, err := Parse([]byte(`{"uid":"u3"}`)); err == nil {
		t.Fatal("want error for missing accessToken")
	}
	if _, err := Parse([]byte(``)); err == nil {
		t.Fatal("want error for empty storage")
	}
}

func TestSaveAtomicRoundtripPreservesSOLOFields(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "trae-u1.json")
	a := &Auth{
		AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1786805537,
		Domain: "trae.cn", ApiHost: "https://api.trae.com.cn",
		MachineID: "m123", DeviceID: "d456",
		UID: "u1", EnterpriseID: "e1", Nickname: "n1", FilePath: fp,
	}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(fp + ".tmp"); !os.IsNotExist(err) {
		t.Error("tmp file should not remain")
	}
	if fi, err := os.Stat(fp); err != nil {
		t.Errorf("file stat err=%v", err)
	} else if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode=%v want 0600", fi.Mode().Perm())
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	b, err := Parse(raw)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if b.AccessToken != "at" || b.UID != "u1" || b.EnterpriseID != "e1" {
		t.Errorf("roundtrip: %+v", b)
	}
	if b.MachineID != "m123" || b.DeviceID != "d456" || b.ApiHost != "https://api.trae.com.cn" {
		t.Errorf("SOLO fields lost: %+v", b)
	}
}

func TestLoadDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "trae-u1.json"), []byte(existingFormat), 0o600)
	os.WriteFile(filepath.Join(dir, "trae-u2.json"), []byte(`{"account":{"uid":"u2"},"auth":{"accessToken":"at2","refreshToken":"r"}}`), 0o600)
	os.WriteFile(filepath.Join(dir, "trae-bad.json"), []byte(`not json`), 0o600)
	os.WriteFile(filepath.Join(dir, "other-ignored.json"), []byte(existingFormat), 0o600)

	list, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 trae accounts, got %+v", list)
	}
	if list[0].FilePath == "" {
		t.Error("FilePath not set")
	}
}

func TestNeedsRefresh(t *testing.T) {
	a := &Auth{ExpiresAt: 0}
	if !a.NeedsRefresh(0) {
		t.Error("zero expiry should need refresh")
	}
	a.ExpiresAt = 9999999999
	if a.NeedsRefresh(0) {
		t.Error("far future should not need refresh")
	}
	a.ExpiresAt = 1
	if !a.NeedsRefresh(24 * 3600 * 1e9) {
		t.Error("past expiry should need refresh")
	}
}

// TestConcurrentRefreshAndReads 在 -race 下验证 token 字段读写并发安全：
// 并发 RefreshToken（写锁）与 JWT/NeedsRefresh/SaveAtomic（读锁/写锁）无数据竞争。
func TestConcurrentRefreshAndReads(t *testing.T) {
	a := &Auth{
		AccessToken:  "at",
		RefreshToken: "rt",
		ExpiresAt:    time.Now().Add(-time.Minute).Unix(),
		FilePath:     filepath.Join(t.TempDir(), "trae-race.json"),
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { // 模拟 RefreshToken 写路径（改写 token 字段）
			defer wg.Done()
			a.Lock()
			a.AccessToken = "new-at"
			a.RefreshToken = "new-rt"
			a.ExpiresAt = time.Now().Add(time.Hour).Unix()
			a.Unlock()
		}()
		wg.Add(1)
		go func() { // 模拟读路径
			defer wg.Done()
			_ = a.JWT()
			_ = a.RefreshTokenValue()
			_ = a.NeedsRefresh(time.Hour)
		}()
	}
	wg.Add(1)
	go func() { // 模拟并发落盘（写锁）
		defer wg.Done()
		_ = a.SaveAtomic()
	}()
	wg.Wait()
	if a.RefreshTokenValue() == "" {
		t.Error("refresh token should not be empty")
	}
}

// TestNeedsRefreshLocked 验证持锁内部版本与外部版本等价。
func TestNeedsRefreshLocked(t *testing.T) {
	a := &Auth{ExpiresAt: time.Now().Add(-time.Minute).Unix()}
	a.mu.RLock()
	need := a.NeedsRefreshLocked(time.Hour)
	a.mu.RUnlock()
	if !need {
		t.Error("expired token should need refresh under lock")
	}
}
