package panel

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRingRetainsTaskDuringChatFloodAndRestores(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "task-logs.json")
	r := NewRing(5)
	if err := r.SetTaskArchive(archive); err != nil {
		t.Fatal(err)
	}
	r.Write([]byte("panel: task auto uid=test code=chat_5 status=done claimed=true\n"))
	for i := 0; i < 50; i++ {
		r.Write([]byte(fmt.Sprintf("| #%d | chat\n", i)))
	}
	counts := map[string]int{}
	for _, e := range r.Snapshot() {
		counts[e.Ch]++
	}
	if counts[ChTask] != 1 || counts[ChChat] != 5 {
		t.Fatalf("channels=%v", counts)
	}
	restored := NewRing(5)
	if err := restored.SetTaskArchive(archive); err != nil {
		t.Fatal(err)
	}
	entries := restored.Snapshot()
	if len(entries) != 1 || !strings.Contains(entries[0].Text, "claimed=true") {
		t.Fatalf("restored=%v", entries)
	}
	for i := 0; i < 12; i++ {
		restored.Write([]byte(fmt.Sprintf("panel: 任务 result %d\n", i)))
	}
	restarted := NewRing(5)
	if err := restarted.SetTaskArchive(archive); err != nil {
		t.Fatal(err)
	}
	if len(restarted.Snapshot()) != 5 {
		t.Fatal("task archive is not bounded")
	}
}

func TestRingPersistenceFailureStillRetainsLogs(t *testing.T) {
	r := NewRing(3)
	archive := filepath.Join(t.TempDir(), "task-logs.json")
	if err := r.SetTaskArchive(archive); err != nil {
		t.Fatal(err)
	}
	// A directory at the file path prevents atomic replacement.
	if err := os.Mkdir(archive, 0700); err != nil {
		t.Fatal(err)
	}
	n, err := r.Write([]byte("panel: 任务 result\n"))
	if err != nil || n == 0 || len(r.Snapshot()) != 1 || r.ArchiveError() == "" {
		t.Fatalf("n=%d err=%v archive=%q", n, err, r.ArchiveError())
	}
	p := New(Config{})
	p.logs = r
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/logs", nil))
	var data struct {
		ArchiveError string `json:"archive_error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if data.ArchiveError == "" {
		t.Fatal("archive error was hidden from the panel API")
	}
}

func TestClassifyLine(t *testing.T) {
	cases := map[string]string{
		"| #001 | glm-5.2 | stream | 200 | uid=c8a3e793 | TTFB=120ms |": ChChat,
		"school c8a3e793: ★ 分享任务完成":                                     ChTask,
		"streak-bonus 5c162cc9: 🎊 新手礼包 +100c":                           ChTask,
		"blackcat c8a3e793: 完成 3 次夜间对话":                                 ChTask,
		"checkin 5c162cc9: 已签到":                                         ChTask,
		"panel: 任务动作 uid=x code=chat_5":                                 ChTask,
		"panel: task auto uid=x code=chat_5 status=claim_pending":       ChTask,
		"panel: 队列启动：6 项（并发 2）":                                         ChTask,
		"panel: revive uid=x":                                           ChSys,
		"workbuddy2api listening on :7863":                              ChSys,
		"scheduler: 余额后台刷新每 5m0s":                                       ChSys,
	}
	for line, want := range cases {
		if got := classifyLine(line); got != want {
			t.Errorf("classifyLine(%q)=%q want %q", line, got, want)
		}
	}
}

func TestRingWriteStripsTimestamp(t *testing.T) {
	r := NewRing(4)
	if _, err := r.Write([]byte("2026/09/14 00:12:34 school x: done\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write([]byte("| #002 | glm | stream | 200 | ok |")); err != nil {
		t.Fatal(err)
	}
	es := r.Snapshot()
	if len(es) != 2 {
		t.Fatalf("entries=%d want 2", len(es))
	}
	if strings.HasPrefix(es[0].Text, "2026/") {
		t.Errorf("timestamp not stripped: %q", es[0].Text)
	}
	if es[0].Ch != ChTask || es[1].Ch != ChChat {
		t.Errorf("channels: %q %q", es[0].Ch, es[1].Ch)
	}
	if time.Since(es[0].TS) > 5*time.Second {
		t.Errorf("stale ts: %v", es[0].TS)
	}
}
