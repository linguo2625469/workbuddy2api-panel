package panel

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/credithist"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// creditPanel 构造带积分账本与一个具名账号的面板（鉴权走 test-key）。
func creditPanel(t *testing.T, ledger *credithist.Ledger) *Panel {
	t.Helper()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "小明"})
	return New(Config{Version: "test", APIKey: "test-key", Pool: p, CreditHistory: ledger})
}

func creditGet(t *testing.T, p *Panel, query string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("GET", "/panel/api/credit_history"+query, nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

type creditResp struct {
	Entries []struct {
		Time    string `json:"time"`
		UID     string `json:"uid"`
		Account string `json:"account"`
		Delta   int64  `json:"delta"`
		Before  int64  `json:"before"`
		After   int64  `json:"after"`
	} `json:"entries"`
	Limit int `json:"limit"`
}

// 接口形状：新的在前、字段齐全、昵称由池快照填充（账本只存 uid）。
func TestCreditHistoryEndpointShape(t *testing.T) {
	l := credithist.New("", 100)
	l.Observe("u1", 1000) // 基线
	l.Observe("u1", 1100) // +100
	l.Observe("u2", 5)    // 无昵称账号：基线
	l.Observe("u2", 25)   // +20（晚于 u1）

	code, body := creditGet(t, creditPanel(t, l), "?limit=10")
	if code != 200 {
		t.Fatalf("GET credit_history -> %d %s", code, body)
	}
	var out creditResp
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, body)
	}
	if out.Limit != 10 {
		t.Fatalf("limit 应回显 10，得到 %d", out.Limit)
	}
	if len(out.Entries) != 2 {
		t.Fatalf("应有 2 条流水（基线不记），得到 %d: %s", len(out.Entries), body)
	}
	if out.Entries[0].UID != "u2" || out.Entries[0].Delta != 20 || out.Entries[0].Before != 5 || out.Entries[0].After != 25 {
		t.Fatalf("新的应在前且字段齐全，得到 %+v", out.Entries[0])
	}
	if out.Entries[1].UID != "u1" || out.Entries[1].Delta != 100 || out.Entries[1].Before != 1000 || out.Entries[1].After != 1100 {
		t.Fatalf("u1 流水不符，得到 %+v", out.Entries[1])
	}
	if out.Entries[1].Account != "小明" {
		t.Fatalf("昵称应由池快照填充，得到 %q", out.Entries[1].Account)
	}
	if out.Entries[0].Account != "" {
		t.Fatalf("池内无此账号时应回空串，得到 %q", out.Entries[0].Account)
	}
	if out.Entries[0].Time == "" {
		t.Fatal("time 不应为空")
	}
}

// limit 口径与 requestLogs 一致：默认 200、上限 1000、非法值回落默认。
func TestCreditHistoryLimitClamp(t *testing.T) {
	l := credithist.New("", 100)
	l.Observe("u1", 0)
	for i := int64(1); i <= 5; i++ {
		l.Observe("u1", i*10)
	}
	p := creditPanel(t, l)
	cases := []struct {
		query string
		want  int
	}{
		{"", 200},
		{"?limit=2", 2},
		{"?limit=5000", 1000},
		{"?limit=0", 200},
		{"?limit=abc", 200},
		{"?limit=-3", 200},
	}
	for _, c := range cases {
		code, body := creditGet(t, p, c.query)
		if code != 200 {
			t.Fatalf("credit_history%s -> %d %s", c.query, code, body)
		}
		var out creditResp
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		if out.Limit != c.want {
			t.Fatalf("credit_history%s limit=%d want %d", c.query, out.Limit, c.want)
		}
	}
}

// uid 过滤按精确匹配：先全量取回再筛，不会被 limit 截断掉目标账号的流水。
func TestCreditHistoryUIDFilter(t *testing.T) {
	l := credithist.New("", 100)
	l.Observe("u1", 0)
	l.Observe("u2", 0)
	l.Observe("u1", 10)
	l.Observe("u2", 20)
	code, body := creditGet(t, creditPanel(t, l), "?uid=u1&limit=1")
	if code != 200 {
		t.Fatalf("-> %d %s", code, body)
	}
	var out creditResp
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) != 1 || out.Entries[0].UID != "u1" || out.Entries[0].Delta != 10 {
		t.Fatalf("uid 过滤结果不符: %+v", out.Entries)
	}
}

// 空账本必须回 []（而不是 null）：前端把 null 与"接口不可用"混在一起会显示成加载失败。
func TestCreditHistoryEmptyReturnsArray(t *testing.T) {
	code, body := creditGet(t, creditPanel(t, credithist.New("", 100)), "")
	if code != 200 {
		t.Fatalf("-> %d %s", code, body)
	}
	if !strings.Contains(body, `"entries":[]`) {
		t.Fatalf("空结果应为 []，得到 %s", body)
	}
}

// 账本未装配 = 501（与 RequestLog 同语义），不是 500 也不是空列表——
// "功能不可用"与"没有记录"必须在协议层可区分。
func TestCreditHistoryNotConfigured(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pool.New("")})
	code, body := creditGet(t, p, "")
	if code != 501 {
		t.Fatalf("未装配账本应回 501，得到 %d %s", code, body)
	}
	if !strings.Contains(body, "credit history not available") {
		t.Fatalf("501 文案不符: %s", body)
	}
}

// 接口与其他 /panel/api/* 同口径鉴权：不带 key 必须 401。
func TestCreditHistoryRequiresAuth(t *testing.T) {
	p := creditPanel(t, credithist.New("", 100))
	req := httptest.NewRequest("GET", "/panel/api/credit_history", nil)
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != 401 {
		t.Fatalf("无 key 应回 401，得到 %d", rr.Code)
	}
}
