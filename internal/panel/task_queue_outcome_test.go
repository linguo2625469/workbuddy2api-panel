package panel

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestGrowthQueuePreservesClaimFailure(t *testing.T) {
	p, account, state := newAutoTaskTestPanel(t, 5, 5, "completed")
	state.claimError = true
	oldGap := reportGap
	reportGap = 0
	t.Cleanup(func() { reportGap = oldGap })
	q := p.queue()
	q.running = true
	q.items = []queueItem{{UID: account.UID, Kind: "growth", Code: "chat_5", Status: "pending"}}
	p.runQueueItems([]queueAccount{{a: account, grow: []upstream.Task{{TaskCode: "chat_5"}}}}, q.items, 1)
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.running || len(q.items) != 1 || q.items[0].Status != "claim_pending" {
		t.Fatalf("queue=%+v", q.items)
	}
	_, claims, _ := state.counters()
	if claims != 1 {
		t.Fatalf("claims=%d", claims)
	}
}
