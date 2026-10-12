package panel

// T4 升级场景集成测试（面板侧）。
//
// 覆盖：老部署升级首启只建基线 / 第二次查询双向留痕 / 升级后新增账号（导入路径）
// 不污染老基线 / 多账号快照互相独立 / 重启续接 / 企业版额度与不限量哨兵 /
// 上游失败不留痕 / 二进制级升级 smoke。
//
// 硬约束：只读业务代码，不改任何生产文件；全部临时文件走 t.TempDir()。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/credithist"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// ---------------------------------------------------------------------------
// 假上游：按 accessToken 识别账号，可分别控制个人版余额、企业版已用/额度与业务错误。
// ---------------------------------------------------------------------------

const upgFakeSize = int64(1) << 40

type upgFakeBilling struct {
	mu      sync.Mutex
	tokens  map[string]string // accessToken -> uid
	remain  map[string]int64  // uid -> 个人版 CycleCapacityRemain
	entUsed map[string]int64  // uid -> 企业版 credit（已用）
	entLim  map[string]int64  // uid -> 企业版 limitNum（额度）
	fail    map[string]bool   // uid -> 返回业务错误
	calls   map[string]int    // uid -> 请求次数
	srv     *httptest.Server
}

func upgNewFakeBilling(t *testing.T) *upgFakeBilling {
	t.Helper()
	f := &upgFakeBilling{
		tokens:  map[string]string{},
		remain:  map[string]int64{},
		entUsed: map[string]int64{},
		entLim:  map[string]int64{},
		fail:    map[string]bool{},
		calls:   map[string]int{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *upgFakeBilling) register(uid string, remain int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens["at-"+uid] = uid
	f.remain[uid] = remain
}

func (f *upgFakeBilling) setPersonal(uid string, remain int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.remain[uid] = remain
}

func (f *upgFakeBilling) setFail(uid string, fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[uid] = fail
}

func (f *upgFakeBilling) registerEnterprise(uid string, used, limit int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens["at-"+uid] = uid
	f.entUsed[uid] = used
	f.entLim[uid] = limit
}

func (f *upgFakeBilling) setEnterprise(uid string, used, limit int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entUsed[uid] = used
	f.entLim[uid] = limit
}

func (f *upgFakeBilling) callCount(uid string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[uid]
}

func (f *upgFakeBilling) handle(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	uid := f.tokens[tok]
	f.calls[uid]++
	fail := f.fail[uid]
	remain := f.remain[uid]
	used := f.entUsed[uid]
	limit := f.entLim[uid]
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasSuffix(r.URL.Path, "/get-enterprise-user-usage"):
		if fail {
			io.WriteString(w, `{"code":400,"msg":"bad request"}`)
			return
		}
		fmt.Fprintf(w, `{"code":0,"msg":"OK","data":{"credit":%d,"limitNum":%d}}`, used, limit)
	case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
		if fail {
			io.WriteString(w, `{"code":400,"msg":"bad request"}`)
			return
		}
		fmt.Fprintf(w, `{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":%d,"CycleCapacityRemain":%d,"CycleCapacityUsed":0}]}}}}`, upgFakeSize, remain)
	case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
		io.WriteString(w, `{"code":0,"msg":"ok","data":{}}`)
	default:
		http.Error(w, "not found", 404)
	}
}

// ---------------------------------------------------------------------------
// 升级环境装配：老部署的 auths/ + state.json → 新版 pool/upstream/ledger/panel
// ---------------------------------------------------------------------------

type upgSeed struct {
	uid          string
	nickname     string
	enterpriseID string
	domain       string
	realm        string
	credits      int64
}

type upgEnv struct {
	t          *testing.T
	dir        string
	authDir    string
	stateFile  string
	ledgerPath string
	fake       *upgFakeBilling
	pool       *pool.Pool
	up         *upstream.Client
	ledger     *credithist.Ledger
	panel      *Panel
}

// upgNewEnv 按老部署的磁盘形态装配：auths/*.json 与 state.json 先落盘，再走
// auth.LoadDir + pool.SyncToDir（与 cmd/server 启动序一致）。
func upgNewEnv(t *testing.T, seeds ...upgSeed) *upgEnv {
	t.Helper()
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	dataDir := filepath.Join(dir, "data")
	for _, d := range []string{authDir, dataDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fake := upgNewFakeBilling(t)
	accounts := map[string]any{}
	for _, s := range seeds {
		realm := s.realm
		if realm == "" {
			realm = "cn"
		}
		doc := map[string]any{
			"accessToken":  "at-" + s.uid,
			"refreshToken": "rt-" + s.uid,
			"expiresAt":    time.Now().Add(365 * 24 * time.Hour).Unix(),
			"uid":          s.uid,
			"nickname":     s.nickname,
			"realm":        realm,
		}
		if s.enterpriseID != "" {
			doc["enterpriseId"] = s.enterpriseID
		}
		if s.domain != "" {
			doc["domain"] = s.domain
		}
		raw, _ := json.Marshal(doc)
		if err := os.WriteFile(filepath.Join(authDir, "workbuddy-"+s.uid+".json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		accounts[s.uid] = map[string]any{"credits": s.credits}
		fake.register(s.uid, s.credits)
	}
	stateRaw, _ := json.Marshal(map[string]any{"accounts": accounts})
	stateFile := filepath.Join(dataDir, "state.json")
	if err := os.WriteFile(stateFile, stateRaw, 0o600); err != nil {
		t.Fatal(err)
	}

	e := &upgEnv{
		t:          t,
		dir:        dir,
		authDir:    authDir,
		stateFile:  stateFile,
		ledgerPath: filepath.Join(dataDir, "credit-history.json"),
		fake:       fake,
	}
	e.pool = pool.New(stateFile)
	t.Cleanup(e.pool.Close)
	auths, err := auth.LoadDir(authDir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	e.pool.SyncToDir(auths)
	e.up = &upstream.Client{HTTP: fake.srv.Client(), ChatBaseCN: fake.srv.URL, BillingBaseCN: fake.srv.URL}
	e.ledger = credithist.New(e.ledgerPath, 2000)
	e.up.SetCreditObserver(e.ledger.Observe)
	e.panel = New(Config{
		Version:       "test",
		APIKey:        "test-key",
		Pool:          e.pool,
		Upstream:      e.up,
		CreditHistory: e.ledger,
		AuthDir:       e.authDir,
	})
	return e
}

// addAccount 模拟「升级后新增账号」：落 auth 文件 + pool.Add（不走导入/登录）。
func (e *upgEnv) addAccount(uid, nickname string) {
	e.t.Helper()
	doc := map[string]any{
		"accessToken":  "at-" + uid,
		"refreshToken": "rt-" + uid,
		"expiresAt":    time.Now().Add(365 * 24 * time.Hour).Unix(),
		"uid":          uid,
		"nickname":     nickname,
		"realm":        "cn",
	}
	raw, _ := json.Marshal(doc)
	path := filepath.Join(e.authDir, "workbuddy-"+uid+".json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		e.t.Fatal(err)
	}
	a, err := auth.Parse(raw)
	if err != nil {
		e.t.Fatal(err)
	}
	a.FilePath = path
	e.fake.register(uid, 0)
	e.pool.Add(a)
}

// balance 走面板单号余额刷新接口，触发一次真实上游余额查询（观察者由此留痕）。
func (e *upgEnv) balance(uid string) (int, string) {
	e.t.Helper()
	return upgPost(e.t, e.panel, "/panel/api/accounts/"+uid+"/balance")
}

// importCockpit 走面板导入路径（cockpit multipart），导入成功后上游会顺带查一次余额。
func (e *upgEnv) importCockpit(accounts []map[string]any) (int, string) {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "accounts.json")
	if err != nil {
		e.t.Fatal(err)
	}
	raw, _ := json.Marshal(accounts)
	if _, err := fw.Write(raw); err != nil {
		e.t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		e.t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/panel/api/import/cockpit", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer test-key")
	rr := httptest.NewRecorder()
	e.panel.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

// ---------------------------------------------------------------------------
// 通用断言辅助
// ---------------------------------------------------------------------------

func upgGet(t *testing.T, p *Panel, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

func upgPost(t *testing.T, p *Panel, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("POST", path, nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

type upgStateAccount struct {
	Credits int64 `json:"credits"`
}

type upgStateFile struct {
	Accounts map[string]upgStateAccount `json:"accounts"`
}

func upgReadState(t *testing.T, path string) upgStateFile {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 state.json: %v", err)
	}
	var sf upgStateFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatalf("state.json 不可解析（被破坏？）: %v\n%s", err, raw)
	}
	return sf
}

type upgDiskLedger struct {
	Snapshot map[string]int64 `json:"snapshot"`
	Entries  []struct {
		Time   string `json:"time"`
		UID    string `json:"uid"`
		Delta  int64  `json:"delta"`
		Before int64  `json:"before"`
		After  int64  `json:"after"`
	} `json:"entries"`
}

func upgReadLedger(t *testing.T, path string) upgDiskLedger {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 credit-history.json: %v", err)
	}
	var d upgDiskLedger
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("credit-history.json 不可解析: %v\n%s", err, raw)
	}
	return d
}

func upgCreditEntries(t *testing.T, p *Panel, query string) creditResp {
	t.Helper()
	code, body := upgGet(t, p, "/panel/api/credit_history"+query)
	if code != 200 {
		t.Fatalf("GET credit_history%s -> %d %s", query, code, body)
	}
	var out creditResp
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("credit_history 响应非法: %v\n%s", err, body)
	}
	return out
}

// ---------------------------------------------------------------------------
// A. 升级首启：账本不存在 + 已有账号 → 第一次真实查询只建基线
// ---------------------------------------------------------------------------

func TestUpgUpgradeFirstStartBuildsBaselineOnly(t *testing.T) {
	e := upgNewEnv(t, upgSeed{uid: "u1", nickname: "老账号", credits: 1300})

	if _, err := os.Stat(e.ledgerPath); !os.IsNotExist(err) {
		t.Fatalf("升级前不应存在账本文件，stat err=%v", err)
	}
	st, ok := e.pool.Status("u1")
	if !ok || st.Credits != 1300 {
		t.Fatalf("老 state.json 的 credits 未加载: ok=%v status=%+v", ok, st)
	}

	e.fake.setPersonal("u1", 1300) // 与旧部署最后一次余额相同
	code, body := e.balance("u1")
	if code != 200 {
		t.Fatalf("升级后首次余额查询 -> %d %s", code, body)
	}
	if n := len(e.ledger.Read(0)); n != 0 {
		t.Fatalf("首次见到账号只应建基线，得到 %d 条流水", n)
	}
	d := upgReadLedger(t, e.ledgerPath)
	if d.Snapshot["u1"] != 1300 {
		t.Fatalf("credit-history.json 快照应含 u1=1300，得到 %+v", d.Snapshot)
	}
	if len(d.Entries) != 0 {
		t.Fatalf("基线不应产生流水，得到 %+v", d.Entries)
	}

	out := upgCreditEntries(t, e.panel, "?limit=10")
	if len(out.Entries) != 0 {
		t.Fatalf("面板接口应回空流水，得到 %+v", out.Entries)
	}
	code, body = upgGet(t, e.panel, "/panel/api/credit_history?limit=10")
	if code != 200 || !strings.Contains(body, `"entries":[]`) {
		t.Fatalf("空结果必须是 []（不是 null）: %d %s", code, body)
	}

	sf := upgReadState(t, e.stateFile)
	if _, ok := sf.Accounts["u1"]; !ok {
		t.Fatalf("升级后旧 state.json 账号丢失: %+v", sf.Accounts)
	}
}

// ---------------------------------------------------------------------------
// B. 第二次查询：+N / −N 各记一条，不变不记
// ---------------------------------------------------------------------------

func TestUpgSecondQueryRecordsDeltaBothDirections(t *testing.T) {
	e := upgNewEnv(t, upgSeed{uid: "u1", credits: 1300})

	e.fake.setPersonal("u1", 1300)
	if code, body := e.balance("u1"); code != 200 {
		t.Fatalf("baseline -> %d %s", code, body)
	}
	e.fake.setPersonal("u1", 1400)
	if code, body := e.balance("u1"); code != 200 {
		t.Fatalf("+100 -> %d %s", code, body)
	}
	e.fake.setPersonal("u1", 1370)
	if code, body := e.balance("u1"); code != 200 {
		t.Fatalf("-30 -> %d %s", code, body)
	}
	e.fake.setPersonal("u1", 1370) // 不变
	if code, body := e.balance("u1"); code != 200 {
		t.Fatalf("no-change -> %d %s", code, body)
	}

	entries := e.ledger.Read(0)
	if len(entries) != 2 {
		t.Fatalf("应有 2 条流水（+100 / -30），得到 %d: %+v", len(entries), entries)
	}
	// Read 新的在前
	if entries[0].Delta != -30 || entries[0].Before != 1400 || entries[0].After != 1370 {
		t.Fatalf("最新一条应为 -30（1400→1370），得到 %+v", entries[0])
	}
	if entries[1].Delta != 100 || entries[1].Before != 1300 || entries[1].After != 1400 {
		t.Fatalf("较早一条应为 +100（1300→1400），得到 %+v", entries[1])
	}

	out := upgCreditEntries(t, e.panel, "?limit=10")
	if len(out.Entries) != 2 || out.Entries[0].Delta != -30 || out.Entries[0].UID != "u1" {
		t.Fatalf("面板流水不符: %+v", out.Entries)
	}
}

// ---------------------------------------------------------------------------
// C. 升级后新增账号（导入路径）：新账号只建基线，老账号基线不被污染
// ---------------------------------------------------------------------------

func TestUpgImportNewAccountKeepsOldBaseline(t *testing.T) {
	e := upgNewEnv(t, upgSeed{uid: "u1", nickname: "老账号", credits: 1000})

	e.fake.setPersonal("u1", 1000)
	if code, body := e.balance("u1"); code != 200 {
		t.Fatalf("u1 baseline -> %d %s", code, body)
	}

	// 新账号走面板 cockpit 导入路径（内部会顺带查一次余额）
	e.fake.register("u2", 500)
	code, body := e.importCockpit([]map[string]any{{
		"uid":           "u2",
		"nickname":      "新号",
		"access_token":  "at-u2",
		"refresh_token": "rt-u2",
		"expires_at":    time.Now().Add(365 * 24 * time.Hour).UnixMilli(),
	}})
	if code != 200 {
		t.Fatalf("导入 -> %d %s", code, body)
	}
	if !strings.Contains(body, `"imported":1`) {
		t.Fatalf("导入未成功: %s", body)
	}

	d := upgReadLedger(t, e.ledgerPath)
	if d.Snapshot["u2"] != 500 {
		t.Fatalf("新账号应只建基线 500，得到 %+v", d.Snapshot)
	}
	if len(d.Entries) != 0 {
		t.Fatalf("导入新账号不应产生流水，得到 %+v", d.Entries)
	}

	// 老账号随后变化，仍按升级前的基线 1000 记
	e.fake.setPersonal("u1", 1050)
	if code, body := e.balance("u1"); code != 200 {
		t.Fatalf("u1 after import -> %d %s", code, body)
	}
	entries := e.ledger.Read(0)
	if len(entries) != 1 {
		t.Fatalf("应恰好 1 条老账号流水，得到 %d: %+v", len(entries), entries)
	}
	if entries[0].UID != "u1" || entries[0].Delta != 50 || entries[0].Before != 1000 || entries[0].After != 1050 {
		t.Fatalf("老账号基线被污染（before 应为 1000）: %+v", entries[0])
	}

	d = upgReadLedger(t, e.ledgerPath)
	if d.Snapshot["u1"] != 1050 || d.Snapshot["u2"] != 500 {
		t.Fatalf("两号快照应互相独立: %+v", d.Snapshot)
	}
}

// ---------------------------------------------------------------------------
// D. 部分账号先导入、升级后再加新账号：快照互相独立、互不覆盖
// ---------------------------------------------------------------------------

func TestUpgLateAddedAccountSnapshotsIndependent(t *testing.T) {
	e := upgNewEnv(t,
		upgSeed{uid: "u1", credits: 100},
		upgSeed{uid: "u2", credits: 200},
	)

	e.fake.setPersonal("u1", 100)
	if code, body := e.balance("u1"); code != 200 {
		t.Fatalf("u1 baseline -> %d %s", code, body)
	}
	e.fake.setPersonal("u2", 200)
	if code, body := e.balance("u2"); code != 200 {
		t.Fatalf("u2 baseline -> %d %s", code, body)
	}

	// 升级后再加 u3（非导入路径）
	e.addAccount("u3", "三号")
	e.fake.setPersonal("u3", 300)
	if code, body := e.balance("u3"); code != 200 {
		t.Fatalf("u3 baseline -> %d %s", code, body)
	}
	if n := len(e.ledger.Read(0)); n != 0 {
		t.Fatalf("三个账号首次查询都应只建基线，得到 %d 条", n)
	}

	e.fake.setPersonal("u1", 150)
	e.fake.setPersonal("u2", 250)
	e.fake.setPersonal("u3", 330)
	for _, uid := range []string{"u1", "u2", "u3"} {
		if code, body := e.balance(uid); code != 200 {
			t.Fatalf("%s change -> %d %s", uid, code, body)
		}
	}

	entries := e.ledger.Read(0)
	if len(entries) != 3 {
		t.Fatalf("应 3 条流水，得到 %d: %+v", len(entries), entries)
	}
	wantBefore := map[string]int64{"u1": 100, "u2": 200, "u3": 300}
	wantDelta := map[string]int64{"u1": 50, "u2": 50, "u3": 30}
	for _, en := range entries {
		if wantBefore[en.UID] != en.Before || wantDelta[en.UID] != en.Delta {
			t.Fatalf("%s 基线/增量不符: %+v（want before=%d delta=%d）",
				en.UID, en, wantBefore[en.UID], wantDelta[en.UID])
		}
	}

	d := upgReadLedger(t, e.ledgerPath)
	if d.Snapshot["u1"] != 150 || d.Snapshot["u2"] != 250 || d.Snapshot["u3"] != 330 {
		t.Fatalf("磁盘快照互相覆盖: %+v", d.Snapshot)
	}

	// 重启读盘后仍一致
	l2 := credithist.New(e.ledgerPath, 2000)
	if n := len(l2.Read(0)); n != 3 {
		t.Fatalf("重启后流水条数 %d want 3", n)
	}
}

// ---------------------------------------------------------------------------
// E. 重启续接：同路径新建 Ledger + pool，基线不丢、同值不记
// ---------------------------------------------------------------------------

func TestUpgRestartContinuesBaseline(t *testing.T) {
	e := upgNewEnv(t, upgSeed{uid: "u1", credits: 100})

	e.fake.setPersonal("u1", 100)
	if code, body := e.balance("u1"); code != 200 {
		t.Fatalf("baseline -> %d %s", code, body)
	}
	e.fake.setPersonal("u1", 150)
	if code, body := e.balance("u1"); code != 200 {
		t.Fatalf("+50 -> %d %s", code, body)
	}
	e.pool.Close() // 模拟进程退出：Flush state.json

	// 重启：同路径重建 Ledger + pool（与 main 的装配一致）
	l2 := credithist.New(e.ledgerPath, 2000)
	if n := len(l2.Read(0)); n != 1 {
		t.Fatalf("重启后应恢复 1 条流水，得到 %d", n)
	}
	p2 := pool.New(e.stateFile)
	t.Cleanup(p2.Close)
	auths, err := auth.LoadDir(e.authDir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	p2.SyncToDir(auths)
	st, ok := p2.Status("u1")
	if !ok || st.Credits != 150 {
		t.Fatalf("重启后余额快照应保留 150，得到 ok=%v %+v", ok, st)
	}

	up2 := &upstream.Client{HTTP: e.fake.srv.Client(), ChatBaseCN: e.fake.srv.URL, BillingBaseCN: e.fake.srv.URL}
	up2.SetCreditObserver(l2.Observe)
	pn2 := New(Config{Version: "test", APIKey: "test-key", Pool: p2, Upstream: up2, CreditHistory: l2, AuthDir: e.authDir})

	// 同值不记
	e.fake.setPersonal("u1", 150)
	if code, body := upgPost(t, pn2, "/panel/api/accounts/u1/balance"); code != 200 {
		t.Fatalf("restart same value -> %d %s", code, body)
	}
	if n := len(l2.Read(0)); n != 1 {
		t.Fatalf("重启后同值不应新增流水，得到 %d", n)
	}
	// 变化按重启前基线记
	e.fake.setPersonal("u1", 160)
	if code, body := upgPost(t, pn2, "/panel/api/accounts/u1/balance"); code != 200 {
		t.Fatalf("restart change -> %d %s", code, body)
	}
	entries := l2.Read(0)
	if len(entries) != 2 {
		t.Fatalf("重启后变化应记一条，得到 %d: %+v", len(entries), entries)
	}
	if entries[0].Delta != 10 || entries[0].Before != 150 || entries[0].After != 160 {
		t.Fatalf("重启续接的 before 应为 150，得到 %+v", entries[0])
	}
}

// ---------------------------------------------------------------------------
// F. 企业版：额度变动记一条；不限量哨兵不产生假流水
// ---------------------------------------------------------------------------

func TestUpgEnterpriseQuotaDelta(t *testing.T) {
	e := upgNewEnv(t, upgSeed{uid: "ue", nickname: "企业号", enterpriseID: "ent-1", credits: 2000})
	e.fake.registerEnterprise("ue", 0, 2000) // credit=0, limitNum=2000 → remain=2000

	if code, body := e.balance("ue"); code != 200 {
		t.Fatalf("企业版 baseline -> %d %s", code, body)
	}
	if n := len(e.ledger.Read(0)); n != 0 {
		t.Fatalf("企业版首查只应建基线，得到 %d", n)
	}

	e.fake.setEnterprise("ue", 100, 2000) // remain=1900
	if code, body := e.balance("ue"); code != 200 {
		t.Fatalf("企业版 1900 -> %d %s", code, body)
	}
	entries := e.ledger.Read(0)
	if len(entries) != 1 {
		t.Fatalf("企业额度 2000→1900 应记 1 条，得到 %d: %+v", len(entries), entries)
	}
	if entries[0].Delta != -100 || entries[0].Before != 2000 || entries[0].After != 1900 {
		t.Fatalf("企业版流水不符: %+v", entries[0])
	}
}

func TestUpgEnterpriseUnlimitedSentinelNoFakeEntry(t *testing.T) {
	e := upgNewEnv(t, upgSeed{uid: "uu", enterpriseID: "ent-2", credits: 0})
	e.fake.registerEnterprise("uu", 0, -1) // limitNum=-1 → remain=1<<40 哨兵

	if code, body := e.balance("uu"); code != 200 {
		t.Fatalf("不限量 baseline -> %d %s", code, body)
	}
	if code, body := e.balance("uu"); code != 200 {
		t.Fatalf("不限量重复查询 -> %d %s", code, body)
	}
	if n := len(e.ledger.Read(0)); n != 0 {
		t.Fatalf("不限量同值不应留痕，得到 %d", n)
	}
	// 从不限量切到有限额：巨大跳变属脏数据，只更新基线
	e.fake.setEnterprise("uu", 0, 2000)
	if code, body := e.balance("uu"); code != 200 {
		t.Fatalf("切换有限额 -> %d %s", code, body)
	}
	if n := len(e.ledger.Read(0)); n != 0 {
		t.Fatalf("哨兵跳变不应留痕，得到 %d", n)
	}
	d := upgReadLedger(t, e.ledgerPath)
	if d.Snapshot["uu"] != 2000 {
		t.Fatalf("哨兵跳变后基线应更新为 2000，得到 %+v", d.Snapshot)
	}
}

// ---------------------------------------------------------------------------
// G. 上游失败：不记流水；恢复后恰好记一次
// ---------------------------------------------------------------------------

func TestUpgUpstreamFailureLeavesNoTrace(t *testing.T) {
	e := upgNewEnv(t, upgSeed{uid: "u1", credits: 1000})

	e.fake.setPersonal("u1", 1000)
	if code, body := e.balance("u1"); code != 200 {
		t.Fatalf("baseline -> %d %s", code, body)
	}

	// 业务错误（非瞬时，避免触发 2s/4s 有界重试）
	e.fake.setFail("u1", true)
	code, body := e.balance("u1")
	if code != 502 {
		t.Fatalf("上游失败应回 502，得到 %d %s", code, body)
	}
	if n := len(e.ledger.Read(0)); n != 0 {
		t.Fatalf("失败不应留痕，得到 %d 条", n)
	}

	e.fake.setFail("u1", false)
	e.fake.setPersonal("u1", 1100)
	if code, body := e.balance("u1"); code != 200 {
		t.Fatalf("恢复后 -> %d %s", code, body)
	}
	entries := e.ledger.Read(0)
	if len(entries) != 1 {
		t.Fatalf("恢复后应恰好 1 条流水（不重复），得到 %d: %+v", len(entries), entries)
	}
	if entries[0].Delta != 100 || entries[0].Before != 1000 || entries[0].After != 1100 {
		t.Fatalf("恢复后流水不符: %+v", entries[0])
	}
}

// ---------------------------------------------------------------------------
// I. 二进制级升级 smoke：旧二进制 → 新二进制，同目录、不同端口
// ---------------------------------------------------------------------------

func upgRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller 失败")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("无法从 %s 定位仓库根: %v", file, err)
	}
	return root
}

func upgBuild(t *testing.T, srcDir, out string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, "./cmd/server")
	cmd.Dir = srcDir
	combined, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build (%s) 失败: %v\n%s", srcDir, err, combined)
	}
}

type upgSafeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *upgSafeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *upgSafeBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type upgProc struct {
	cmd  *exec.Cmd
	log  *upgSafeBuf
	once sync.Once
}

func upgStart(t *testing.T, exe, workDir, cfgPath string) *upgProc {
	t.Helper()
	p := &upgProc{log: &upgSafeBuf{}}
	p.cmd = exec.Command(exe, "-config", cfgPath)
	p.cmd.Dir = workDir
	p.cmd.Stdout = p.log
	p.cmd.Stderr = p.log
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("启动 %s 失败: %v", exe, err)
	}
	t.Cleanup(p.kill)
	return p
}

func (p *upgProc) kill() {
	p.once.Do(func() {
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
			_ = p.cmd.Wait()
		}
	})
}

func upgFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func upgWriteConfig(t *testing.T, path string, port int, base, authDir, stateFile string) {
	t.Helper()
	cfg := map[string]any{
		"listen":     fmt.Sprintf("127.0.0.1:%d", port),
		"api_key":    "smoke-key",
		"auth_dir":   authDir,
		"state_file": stateFile,
		"global": map[string]any{
			"enabled":      true,
			"chat_base":    base,
			"billing_base": base,
		},
		"schedule": map[string]any{
			"checkin_enabled":         false,
			"travel_enabled":          false,
			"activity_enabled":        false,
			"keepalive_enabled":       false,
			"blackcat_enabled":        false,
			"growth_enabled":          false,
			"balance_refresh_enabled": false,
		},
	}
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func upgWaitHTTP(t *testing.T, method, url, key string, want int, timeout time.Duration) (int, string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(method, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == want {
				return resp.StatusCode, string(b)
			}
			last = fmt.Sprintf("status=%d body=%s", resp.StatusCode, b)
		} else {
			last = err.Error()
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("等待 %s %s 超时（want %d）: %s", method, url, want, last)
	return 0, ""
}

func upgWaitStateCredits(t *testing.T, path, uid string, want int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			var sf upgStateFile
			if json.Unmarshal(raw, &sf) == nil && sf.Accounts[uid].Credits == want {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("等待 %s 落盘 credits[%s]=%d 超时", path, uid, want)
}

func upgWaitFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("等待文件 %s 生成超时", path)
}

func TestUpgBinaryUpgradeSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("二进制升级 smoke 在 -short 下跳过")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("PATH 中无 go，跳过: %v", err)
	}
	// 旧版源码路径由环境变量提供：仓库里不该硬编码某个人的本地目录。
	// 要跑这个 smoke：
	//   WB2API_UPGRADE_OLD_SRC=<旧版 checkout> go test ./internal/panel/ -run TestUpgBinaryUpgradeSmoke -v
	oldSrc := os.Getenv("WB2API_UPGRADE_OLD_SRC")
	if oldSrc == "" {
		t.Skip("未设置 WB2API_UPGRADE_OLD_SRC，跳过二进制升级 smoke")
	}
	if st, err := os.Stat(oldSrc); err != nil || !st.IsDir() {
		t.Skipf("旧版源码不可用（%s），跳过: %v", oldSrc, err)
	}
	root := upgRepoRoot(t)

	tmp := t.TempDir()
	oldExe := filepath.Join(tmp, "wb2api-old.exe")
	newExe := filepath.Join(tmp, "wb2api-new.exe")
	upgBuild(t, oldSrc, oldExe)
	upgBuild(t, root, newExe)

	deploy := filepath.Join(tmp, "deploy")
	authDir := filepath.Join(deploy, "auths")
	dataDir := filepath.Join(deploy, "data")
	for _, d := range []string{authDir, dataDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	uid := "smoke1"
	authDoc := map[string]any{
		"auth": map[string]any{
			"accessToken":  "at-" + uid,
			"refreshToken": "rt-" + uid,
			"expiresAt":    time.Now().Add(365 * 24 * time.Hour).Unix(),
			"domain":       "workbuddy.ai",
			"realm":        "global",
		},
		"account": map[string]any{"uid": uid, "nickname": "smoke"},
	}
	authRaw, _ := json.MarshalIndent(authDoc, "", "  ")
	if err := os.WriteFile(filepath.Join(authDir, "workbuddy-"+uid+".json"), authRaw, 0o600); err != nil {
		t.Fatal(err)
	}

	fake := upgNewFakeBilling(t)
	fake.register(uid, 1300)
	stateFile := filepath.Join(dataDir, "state.json")
	cfgPath := filepath.Join(deploy, "config.json")

	// --- 旧二进制：启动、真实查一次余额、落 state.json ---
	oldPort := upgFreePort(t)
	upgWriteConfig(t, cfgPath, oldPort, fake.srv.URL, authDir, stateFile)
	oldProc := upgStart(t, oldExe, deploy, cfgPath)
	oldBase := fmt.Sprintf("http://127.0.0.1:%d", oldPort)
	upgWaitHTTP(t, "GET", oldBase+"/panel/", "", 200, 30*time.Second)
	if code, body := upgWaitHTTP(t, "POST", oldBase+"/panel/api/accounts/"+uid+"/balance", "smoke-key", 200, 20*time.Second); code != 200 {
		t.Fatalf("旧版 balance -> %d %s\n旧版日志:\n%s", code, body, oldProc.log.String())
	}
	upgWaitStateCredits(t, stateFile, uid, 1300, 20*time.Second)
	oldProc.kill()

	before := upgReadState(t, stateFile)
	if before.Accounts[uid].Credits != 1300 {
		t.Fatalf("旧版生成的 state.json 不符: %+v", before.Accounts)
	}

	// --- 新二进制：同目录升级、换端口 ---
	newPort := upgFreePort(t)
	upgWriteConfig(t, cfgPath, newPort, fake.srv.URL, authDir, stateFile)
	newProc := upgStart(t, newExe, deploy, cfgPath)
	newBase := fmt.Sprintf("http://127.0.0.1:%d", newPort)
	upgWaitHTTP(t, "GET", newBase+"/panel/", "", 200, 30*time.Second)

	after := upgReadState(t, stateFile)
	if after.Accounts[uid].Credits != 1300 {
		t.Fatalf("升级后旧 state.json 被破坏: %+v\n新版日志:\n%s", after.Accounts, newProc.log.String())
	}

	// 升级前不存在的接口，升级后可用
	code, body := upgWaitHTTP(t, "GET", newBase+"/panel/api/credit_history", "smoke-key", 200, 20*time.Second)
	if !strings.Contains(body, `"entries"`) {
		t.Fatalf("credit_history 响应形状不符: %d %s", code, body)
	}

	// 触发一次真实余额查询 → 建基线并落 credit-history.json
	if code, body := upgWaitHTTP(t, "POST", newBase+"/panel/api/accounts/"+uid+"/balance", "smoke-key", 200, 20*time.Second); code != 200 {
		t.Fatalf("新版 balance -> %d %s\n新版日志:\n%s", code, body, newProc.log.String())
	}
	histPath := filepath.Join(dataDir, "credit-history.json")
	upgWaitFile(t, histPath, 20*time.Second)
	d := upgReadLedger(t, histPath)
	if d.Snapshot[uid] != 1300 {
		t.Fatalf("credit-history.json 基线不符: %+v", d.Snapshot)
	}
	if len(d.Entries) != 0 {
		t.Fatalf("升级后首次查询不应有流水: %+v", d.Entries)
	}
	newProc.kill()
}
