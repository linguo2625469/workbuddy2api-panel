package captcha

// pool.go — pre-solved verify-param stock:
// a background loop keeps POOL_MIN tokens minted (up to POOL_MAX, strictly
// serial — never a burst of node processes), the hot path pops the oldest
// valid token (FIFO by age, TTL-bounded) and fires a fire-and-forget
// restock of one; an upstream challenge drains the whole pool (the batch
// is untrustworthy). Refill only runs while a servable JWT account exists
// (the active gate) — an all-cooling pool generates zero upstream traffic.

import (
	"context"
	"sync"
	"time"
)

type poolToken struct {
	param, region string
	at            time.Time
}

// MintPool wraps a Solver with a pre-solved stock. It implements Solver.
type MintPool struct {
	inner  Solver
	active func() bool // worth minting? (a servable jwt account exists)
	min    int
	max    int
	ttl    time.Duration

	mu     sync.Mutex
	tokens []poolToken
	stop   chan struct{}
	done   sync.WaitGroup
}

func NewMintPool(inner Solver, active func() bool, min, max int, ttl time.Duration) *MintPool {
	if min < 1 {
		min = 1
	}
	if max < min {
		max = min
	}
	if ttl <= 0 {
		ttl = 95 * time.Second
	}
	p := &MintPool{inner: inner, active: active, min: min, max: max, ttl: ttl, stop: make(chan struct{})}
	p.done.Add(1)
	go p.loop()
	return p
}

func (p *MintPool) loop() {
	defer p.done.Done()
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			if p.active == nil || !p.active() {
				p.evictExpired()
				continue
			}
			p.evictExpired()
			need := p.min - p.validLen()
			for i := 0; i < need; i++ {
				if !p.mintOne(context.Background()) {
					break // solver failing — try again on the next tick
				}
			}
		}
	}
}

func (p *MintPool) validLen() int {
	n := 0
	for _, t := range p.tokens {
		if time.Since(t.at) < p.ttl {
			n++
		}
	}
	return n
}

func (p *MintPool) evictExpired() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.evictExpiredLocked()
}

func (p *MintPool) evictExpiredLocked() {
	kept := p.tokens[:0]
	for _, t := range p.tokens {
		if time.Since(t.at) < p.ttl {
			kept = append(kept, t)
		}
	}
	p.tokens = kept
}

// mintOne solves serially and appends when there is room. False = the
// solver failed (the caller backs off until the next tick).
func (p *MintPool) mintOne(ctx context.Context) bool {
	param, region, err := p.inner.Solve(ctx)
	if err != nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.evictExpiredLocked()
	if len(p.tokens) >= p.max {
		return true // solved fine, stock is full; token dropped
	}
	p.tokens = append(p.tokens, poolToken{param: param, region: region, at: time.Now()})
	return true
}

func (p *MintPool) pop() (string, string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.evictExpiredLocked()
	if len(p.tokens) == 0 {
		return "", "", false
	}
	t := p.tokens[0]
	p.tokens = p.tokens[1:]
	return t.param, t.region, true
}

// Solve returns a pre-minted token (oldest first), kicks a background
// restock of one, and falls back to a synchronous solve on an empty pool.
func (p *MintPool) Solve(ctx context.Context) (string, string, error) {
	if param, region, ok := p.pop(); ok {
		p.restock()
		return param, region, nil
	}
	if param, region, err := p.inner.Solve(ctx); err == nil {
		p.restock()
		return param, region, nil
	} else {
		return "", "", err
	}
}

func (p *MintPool) restock() {
	p.done.Add(1)
	go func() {
		defer p.done.Done()
		if p.active != nil && !p.active() {
			return
		}
		p.mintOne(context.Background())
	}()
}

// Invalidate drains the whole stock — an upstream captcha challenge makes
// the current batch untrustworthy.
func (p *MintPool) Invalidate() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tokens = nil
}

func (p *MintPool) Stock() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.validLen()
}

func (p *MintPool) Close() {
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
	p.done.Wait()
	p.inner.Close()
}
