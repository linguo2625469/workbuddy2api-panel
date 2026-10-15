package proxy

import (
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

// ProxyPool round-robins HTTP proxies. Same line format as GLM-Free-API:
// host:port, http://user:pass@host:port, user:pass@host:port.
// The pool is the runtime view of the stored proxy profiles — SetURLs
// swaps it in place whenever a profile is edited.
type ProxyPool struct {
	mu   sync.RWMutex
	urls []*url.URL
	idx  atomic.Uint64
}

// SetURLs atomically replaces the pool contents (dashboard profile edits).
func (p *ProxyPool) SetURLs(urls []*url.URL) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.urls = urls
	p.mu.Unlock()
}

func (p *ProxyPool) snapshot() []*url.URL {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.urls
}

func ParseProxyLine(line string) (*url.URL, error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return nil, fmt.Errorf("empty")
	}
	if !strings.Contains(line, "://") {
		line = "http://" + line
	}
	u, err := url.Parse(line)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("bad proxy %q", line)
	}
	if u.Scheme == "" {
		u.Scheme = "http"
	}
	return u, nil
}

func LoadProxyPool(path string) (*ProxyPool, error) {
	p := &ProxyPool{}
	if path == "" {
		return p, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return p, nil
		}
		return p, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		u, err := ParseProxyLine(line)
		if err != nil {
			continue
		}
		p.urls = append(p.urls, u)
	}
	return p, nil
}

// At returns the i-th proxy URL (nil when out of range).
func (p *ProxyPool) At(i int) *url.URL {
	urls := p.snapshot()
	if i < 0 || i >= len(urls) {
		return nil
	}
	return urls[i]
}

func (p *ProxyPool) Len() int { return len(p.snapshot()) }

func (p *ProxyPool) Next() *url.URL {
	urls := p.snapshot()
	if len(urls) == 0 {
		return nil
	}
	i := p.idx.Add(1) - 1
	return urls[i%uint64(len(urls))]
}

// Sticky policy (manager gateway evidence, 273-request log): ONE random
// upstream holds all traffic until a load verdict or connect death, then a
// random replacement excluding the failed one. Round-robin-per-request is
// what we had before — every request from a new IP reads as a bot pattern.
type StickyProxy struct {
	mu   sync.Mutex
	cur  *url.URL
	Pool *ProxyPool
}

// Next returns the current sticky proxy, picking a random one on first use.
// Current returns the current sticky proxy (nil until first use, and nil
// on a nil receiver).
func (sp *StickyProxy) Current() *url.URL {
	if sp == nil {
		return nil
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.cur
}

func (sp *StickyProxy) Next() *url.URL {
	if sp == nil || sp.Pool == nil || sp.Pool.Len() == 0 {
		return nil
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.cur == nil {
		sp.cur = sp.Pool.Random()
	}
	return sp.cur
}

// Pin forces the initial sticky proxy (experiments / operator override).
func (sp *StickyProxy) Pin(u *url.URL) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	sp.cur = u
}

// Rotate drops the current sticky proxy and picks a random different one.
func (sp *StickyProxy) Rotate(failed *url.URL) *url.URL {
	if sp == nil || sp.Pool == nil || sp.Pool.Len() == 0 {
		return nil
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if failed != nil && sp.cur != nil && sp.cur.String() != failed.String() {
		return sp.cur // someone already rotated
	}
	if next := sp.Pool.RandomExcluding(sp.cur); next != nil {
		sp.cur = next
	}
	return sp.cur
}

// Random returns a uniformly random proxy from the pool.
func (p *ProxyPool) Random() *url.URL {
	urls := p.snapshot()
	if len(urls) == 0 {
		return nil
	}
	return urls[rand.Intn(len(urls))]
}

// RandomExcluding returns a random proxy different from exclude.
func (p *ProxyPool) RandomExcluding(exclude *url.URL) *url.URL {
	urls := p.snapshot()
	if len(urls) == 0 {
		return nil
	}
	bad := ""
	if exclude != nil {
		bad = exclude.String()
	}
	if len(urls) == 1 && urls[0].String() == bad {
		return nil
	}
	for i := 0; i < len(urls); i++ {
		u := urls[rand.Intn(len(urls))]
		if u.String() != bad {
			return u
		}
	}
	return nil
}
