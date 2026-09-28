package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/notify"
)

func newNotifyTestPanel(t *testing.T, chans []notify.Channel) (*Panel, *httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"errcode":0}`))
	}))
	t.Cleanup(upstream.Close)

	n := notify.NewNotifier(upstream.Client(), time.Minute)
	for i := range chans {
		if chans[i].Config == nil {
			chans[i].Config = map[string]any{}
		}
		chans[i].Config["webhook_url"] = upstream.URL
	}
	n.SetChannels(chans)

	pn := New(Config{Notifier: n})
	return pn, upstream, &calls
}

func TestNotifyTestByName(t *testing.T) {
	pn, _, calls := newNotifyTestPanel(t, []notify.Channel{
		{Name: "钉钉群", Type: "wechat", Enabled: true},
	})
	req := httptest.NewRequest("POST", "/panel/api/notify/test", strings.NewReader(`{"name":"钉钉群"}`))
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["ok"] != true {
		t.Fatalf("resp=%v", resp)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls=%d", calls.Load())
	}
}

func TestNotifyTestByAdhocConfig(t *testing.T) {
	pn, _, calls := newNotifyTestPanel(t, nil)
	body := `{"type":"wechat","config":{"webhook_url":"` + "" + `"}}`
	// 临时配置不带 URL 时应报渠道侧错误（走 Notifier.Test → Send 报配置为空）。
	req := httptest.NewRequest("POST", "/panel/api/notify/test", strings.NewReader(body))
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["ok"] != false {
		t.Fatalf("resp=%v（空 webhook_url 应失败）", resp)
	}
	if calls.Load() != 0 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestNotifyTestUnknownChannel(t *testing.T) {
	pn, _, _ := newNotifyTestPanel(t, nil)
	req := httptest.NewRequest("POST", "/panel/api/notify/test", strings.NewReader(`{"name":"不存在"}`))
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404", rec.Code)
	}
}

func TestNotifyTestBadBody(t *testing.T) {
	pn, _, _ := newNotifyTestPanel(t, nil)
	req := httptest.NewRequest("POST", "/panel/api/notify/test", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", rec.Code)
	}
}
