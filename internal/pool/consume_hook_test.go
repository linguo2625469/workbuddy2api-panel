package pool

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestOnConsumeHook(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "小号"})
	p.SetCreditsDetailed("u1", 100, 100, 0, time.Time{}, 0)

	type evt struct {
		nick           string
		consume        float64
		remain, total  int64
	}
	var calls atomic.Int32
	ch := make(chan evt, 4)
	p.SetOnConsume(func(uid, nickname string, consume float64, remain, total int64) {
		calls.Add(1)
		ch <- evt{nickname, consume, remain, total}
	})

	p.NoteModelCost("u1", "m", 7, 1000)
	select {
	case e := <-ch:
		if e.nick != "小号" || e.consume != 7 || e.remain != 93 || e.total != 100 {
			t.Fatalf("evt=%+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("消耗钩子未触发")
	}

	// credit=0（免费请求）不触发。
	p.NoteModelCost("u1", "m", 0, 1000)
	time.Sleep(100 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("calls=%d want 1（credit=0 不应触发）", calls.Load())
	}
}
