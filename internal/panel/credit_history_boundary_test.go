package panel

// T5 接口层边界与故障测试。只新增测试，不改业务代码。

import (
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/credithist"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// limit 口径：默认 200、上限 1000；0 / 负数 / 非法 / 溢出 / 空 一律回落 200。
func TestBndCreditHistoryLimitBoundaries(t *testing.T) {
	l := credithist.New("", 100)
	l.Observe("u1", 0)
	for i := int64(1); i <= 3; i++ {
		l.Observe("u1", i)
	}
	p := creditPanel(t, l)
	cases := []struct {
		query string
		want  int
	}{
		{"", 200},
		{"?limit=", 200},
		{"?limit=0", 200},
		{"?limit=-1", 200},
		{"?limit=abc", 200},
		{"?limit=1e9", 200},
		{"?limit=99999999999999999999", 200}, // Atoi 溢出
		{"?limit=1", 1},
		{"?limit=1000", 1000},
		{"?limit=1001", 1000},
		{"?limit=1000000000", 1000},
	}
	for _, c := range cases {
		code, body := creditGet(t, p, c.query)
		if code != 200 {
			t.Fatalf("credit_history%s -> %d %s", c.query, code, body)
		}
		var out creditResp
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("credit_history%s 响应不是合法 JSON: %v %s", c.query, err, body)
		}
		if out.Limit != c.want {
			t.Fatalf("credit_history%s limit=%d want %d", c.query, out.Limit, c.want)
		}
		if out.Entries == nil {
			t.Fatalf("credit_history%s entries 不应为 null: %s", c.query, body)
		}
	}
}

// uid 过滤：命中 / 不命中 / 空串；且账本条数超过 limit 时，目标账号的较旧流水
// 仍必须被取到（验证服务端先全量取回再过滤，而不是先截断再过滤）。
func TestBndCreditHistoryUIDFilterBoundaries(t *testing.T) {
	l := credithist.New("", 2000)
	l.Observe("u1", 0)
	l.Observe("u2", 0)
	l.Observe("u1", 10) // u1 的变动先发生
	for i := int64(1); i <= 300; i++ {
		l.Observe("u2", i) // 之后是 300 条更新的 u2 流水
	}
	p := creditPanel(t, l)

	// 命中：u1 那条比「最新 100 条」更旧，仍必须返回。
	code, body := creditGet(t, p, "?uid=u1&limit=100")
	if code != 200 {
		t.Fatalf("uid=u1 -> %d %s", code, body)
	}
	var out creditResp
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v %s", err, body)
	}
	if len(out.Entries) != 1 || out.Entries[0].UID != "u1" || out.Entries[0].Delta != 10 {
		t.Fatalf("uid 过滤应取到较旧的 u1 流水: %+v", out.Entries)
	}
	if out.Limit != 100 {
		t.Fatalf("limit 应回显 100，得到 %d", out.Limit)
	}

	// 不命中：必须是 []，不能是 null。
	code, body = creditGet(t, p, "?uid=nope&limit=100")
	if code != 200 {
		t.Fatalf("uid=nope -> %d %s", code, body)
	}
	if !strings.Contains(body, `"entries":[]`) {
		t.Fatalf("不命中应回 []，得到 %s", body)
	}

	// 空串 = 不过滤。
	code, body = creditGet(t, p, "?uid=&limit=5")
	if code != 200 {
		t.Fatalf("uid 空串 -> %d %s", code, body)
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v %s", err, body)
	}
	if len(out.Entries) != 5 {
		t.Fatalf("空 uid 应视为不过滤并回 5 条，得到 %d", len(out.Entries))
	}
}

// 账本满 2000 条时 limit=100 只回 100 条；字段类型必须正确
// （time 为 RFC3339 字符串，delta/before/after 为 JSON 数字）。
func TestBndCreditHistoryFullLedgerFieldTypes(t *testing.T) {
	l := credithist.New("", 2000)
	l.Observe("u1", 0)
	for i := int64(1); i <= 2000; i++ {
		l.Observe("u1", i)
	}
	if got := len(l.Read(0)); got != 2000 {
		t.Fatalf("账本应为 2000 条，得到 %d", got)
	}
	code, body := creditGet(t, creditPanel(t, l), "?limit=100")
	if code != 200 {
		t.Fatalf("-> %d %s", code, body)
	}
	// 用 json.RawMessage 看原始类型，避免反序列化到强类型后把类型错误吞掉。
	var rawResp struct {
		Entries []map[string]json.RawMessage `json:"entries"`
		Limit   int                          `json:"limit"`
	}
	if err := json.Unmarshal([]byte(body), &rawResp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, body)
	}
	if rawResp.Limit != 100 {
		t.Fatalf("limit 应回显 100，得到 %d", rawResp.Limit)
	}
	if len(rawResp.Entries) != 100 {
		t.Fatalf("2000 条账本 limit=100 应只回 100 条，得到 %d", len(rawResp.Entries))
	}
	first := rawResp.Entries[0]
	for _, k := range []string{"time", "uid", "account", "delta", "before", "after"} {
		if _, ok := first[k]; !ok {
			t.Fatalf("字段 %s 缺失: %s", k, body)
		}
	}
	var tstr string
	if err := json.Unmarshal(first["time"], &tstr); err != nil {
		t.Fatalf("time 应为 JSON 字符串: %v", err)
	}
	if _, err := time.Parse(time.RFC3339, tstr); err != nil {
		t.Fatalf("time 应为 RFC3339，得到 %q: %v", tstr, err)
	}
	for _, k := range []string{"delta", "before", "after"} {
		s := strings.TrimSpace(string(first[k]))
		if s == "" || s[0] == '"' {
			t.Fatalf("%s 应为 JSON 数字，得到 %s", k, s)
		}
		if _, err := strconv.ParseInt(s, 10, 64); err != nil {
			t.Fatalf("%s 应为整数，得到 %s", k, s)
		}
	}
	var after0, after1 int64
	if err := json.Unmarshal(first["after"], &after0); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rawResp.Entries[1]["after"], &after1); err != nil {
		t.Fatal(err)
	}
	if after0 != 2000 || after1 != 1999 {
		t.Fatalf("新的应在前: 首条 after=%d 次条 after=%d", after0, after1)
	}
}

// 账本未装配 → 501 固定文案；空账本 → entries=[]；api_key 为空 → 不鉴权。
func TestBndCreditHistoryNilLedgerAndOpenAuth(t *testing.T) {
	// 未装配账本 = 501（与 RequestLog 同语义）。
	missing := New(Config{Version: "test", APIKey: "test-key", Pool: pool.New("")})
	code, body := creditGet(t, missing, "")
	if code != 501 || !strings.Contains(body, "credit history not available") {
		t.Fatalf("nil 账本应回 501 + 固定文案，得到 %d %s", code, body)
	}

	// 空账本 → entries 必须是 []，不是 null。
	l := credithist.New("", 100)
	empty := New(Config{Version: "test", APIKey: "test-key", Pool: pool.New(""), CreditHistory: l})
	code, body = creditGet(t, empty, "")
	if code != 200 || !strings.Contains(body, `"entries":[]`) {
		t.Fatalf("空账本应回 200 且 entries=[]，得到 %d %s", code, body)
	}

	// api_key 为空 = 不启用鉴权（既有语义）：无 Authorization 头也应放行。
	l.Observe("u1", 0)
	l.Observe("u1", 5)
	open := New(Config{Version: "test", Pool: pool.New(""), CreditHistory: l})
	req := httptest.NewRequest("GET", "/panel/api/credit_history", nil)
	rr := httptest.NewRecorder()
	open.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("无 api_key 应放行，得到 %d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"delta":5`) {
		t.Fatalf("应返回流水，得到 %s", rr.Body.String())
	}
}
