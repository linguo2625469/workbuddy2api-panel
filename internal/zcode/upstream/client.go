// Package upstream — the zcode.z.ai origin side: chat transport, billing
// fetch, verdict classification. The gateway (internal/gateway) drives it.
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/zcode/captcha"
	"github.com/linguo2625469/workbuddy2api-panel/internal/zcode/config"
	"github.com/linguo2625469/workbuddy2api-panel/internal/zcode/proxy"
	"github.com/linguo2625469/workbuddy2api-panel/internal/zcode/store"
	"github.com/linguo2625469/workbuddy2api-panel/internal/zcode/util"
)

const (
	messagesPath = "/api/v1/zcode-plan/anthropic/v1/messages"
	balancePath  = "/api/v1/zcode-plan/billing/balance"
)

// Client is the origin-facing half of the server: it mints captcha params
// (direct from this host), sends chat requests over the sticky egress pool
// and fetches billing.
type Client struct {
	Cfg     config.Config
	Captcha captcha.Solver
	Sticky  *proxy.StickyProxy // chat egress: one line holds traffic until it fails
	Pool    *proxy.ProxyPool   // billing-family egress: round-robin spread
	// ProfileForAccount returns a per-account bound proxy URL (dashboard
	// proxy profiles); nil = fall through to the pool.
	ProfileForAccount func(accDeviceMid string) *url.URL

	egMu   sync.Mutex
	egress map[string]*http.Client // billing-family clients, per egress URL
}

// egressURL picks the billing-family egress: the bound dashboard profile
// if any, else the NEXT pool line (round-robin). A 46-account scan must
// spread across the pool — one sticky line hits the provider's per-line
// connection cap ("Not Enough Connections"). Empty pool = nil = direct.
func (c *Client) egressURL(mid string) *url.URL {
	u := (*url.URL)(nil)
	if c.ProfileForAccount != nil {
		u = c.ProfileForAccount(mid)
	}
	if u == nil && c.Pool != nil {
		u = c.Pool.Next()
	}
	return u
}

// clientFor returns the cached billing-family client for an egress URL
// (nil = direct).
func (c *Client) clientFor(u *url.URL) *http.Client {
	key := "direct"
	if u != nil {
		key = u.String()
	}
	c.egMu.Lock()
	defer c.egMu.Unlock()
	if cl := c.egress[key]; cl != nil {
		return cl
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil // direct must be truly direct (no env mitm fallback)
	if u != nil {
		tr.Proxy = http.ProxyURL(u)
	}
	cl := &http.Client{Timeout: 12 * time.Second, Transport: tr}
	if c.egress == nil {
		c.egress = map[string]*http.Client{}
	}
	c.egress[key] = cl
	return cl
}

// egressDo runs a billing-family request on the account's egress; on a
// network-level failure it retries once direct:
// "invalid proxy, falling back to a direct connection").
func (c *Client) egressDo(mid string, req *http.Request) (*http.Response, error) {
	u := c.egressURL(mid)
	resp, err := c.clientFor(u).Do(req)
	if err == nil || u == nil {
		return resp, err
	}
	log.Printf("[billing] egress %s failed: %v — direct retry", u.Host, err)
	if req.GetBody != nil {
		if body, berr := req.GetBody(); berr == nil {
			req.Body = body
		}
	}
	return c.clientFor(nil).Do(req)
}

// IsLoadVerdict reports an upstream LOAD verdict (manager gateway
// LOAD_CODES = {3007, 3009, 3010, 3012}): rotate the egress and retry.
// Status from resp.StatusCode, business code parsed from the body JSON.
func IsLoadVerdict(status int, bodyHead []byte) bool {
	// 529 is always retryable: the manager gateway answers 529 {"overloaded"}
	// after rotating its own DC proxy (gateway.py retryable_error_response),
	// expecting the client to repeat — the IDE's AI SDK does; so do we.
	if status == 529 {
		return true
	}
	if status != 405 && status != 429 && status != 503 {
		return false
	}
	switch originJSONCode(bodyHead) {
	case 3007, 3009, 3010, 3012:
		return true
	}
	return false
}

func billingHeaders(jwt, mid, appVer string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+jwt)
	h.Set("Accept", "*/*")
	h.Set("Accept-Language", "*")
	h.Set("User-Agent", "ZCode/"+appVer)
	h.Set("HTTP-Referer", "https://zcode.z.ai")
	h.Set("X-Title", "Z Code@electron")
	h.Set("X-ZCode-App-Version", appVer)
	// os.release() semantics (fingerprint.py pools), NOT a marketing name
	h.Set("X-Os-Category", "windows")
	h.Set("X-Os-Version", "10.0.26200")
	h.Set("X-Platform", "win32-x64")
	h.Set("X-Release-Channel", "stable")
	h.Set("X-Client-Language", "en-US")
	// same pair the activation events report (language ↔ timezone)
	h.Set("X-Client-Timezone", "America/New_York")
	h.Set("X-Device-Mid", mid)
	h.Set("x-request-id", util.GenerateUUID())
	h.Set("sec-fetch-mode", "cors")
	return h
}

// Billing fetches the account's plan/quota snapshot (billing rides the
// round-robin pool proxy when configured, direct otherwise).
func (c *Client) Billing(jwt, mid string) (store.Quota, error) {
	u := config.OriginBaseURL + balancePath + "?app_version=" + config.BillingClientVersion
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return store.Quota{}, err
	}
	req.Header = billingHeaders(jwt, mid, config.BillingClientVersion)
	resp, err := c.egressDo(mid, req)
	if err != nil {
		return store.Quota{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return store.Quota{}, err
	}
	if resp.StatusCode != 200 {
		return store.Quota{}, fmt.Errorf("billing HTTP %d %s", resp.StatusCode, BodySnip(raw, 200))
	}
	var wrap struct {
		Code int            `json:"code"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return store.Quota{}, err
	}
	if wrap.Code != 0 {
		return store.Quota{}, fmt.Errorf("billing code %d", wrap.Code)
	}
	return quotaFromBalance(wrap.Data), nil
}

func quotaFromBalance(data map[string]any) store.Quota {
	q := store.Quota{PlanName: "Free"}
	if data == nil {
		return q
	}
	plans, _ := data["plans"].([]any)
	if len(plans) > 0 {
		if p, ok := plans[0].(map[string]any); ok {
			if n := Str(p["name"]); n != "" {
				q.PlanName = n
			}
			q.ExpiresAt = unixDate(p["ends_at"])
			if q.ExpiresAt == "" {
				q.ExpiresAt = unixDate(p["expires_at"])
			}
		}
	}
	bals, _ := data["balances"].([]any)
	for _, raw := range bals {
		b, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		q.Balances = append(q.Balances, b)
		q.Entitlements = append(q.Entitlements, map[string]any{
			"name":       firstNonEmpty(Str(b["show_name"]), Str(b["entitlement_id"])),
			"remaining":  AsInt(b["remaining_units"]),
			"used":       AsInt(b["used_units"]),
			"limit":      AsInt(b["total_units"]),
			"period_end": unixInt(b["period_end"]),
		})
		q.Remaining += AsInt(b["remaining_units"])
		q.Used += AsInt(b["used_units"])
		q.Limit += AsInt(b["total_units"])
	}
	return q
}

func unixDate(v any) string {
	n := unixInt(v)
	if n > 0 {
		return time.Unix(n, 0).UTC().Format("2006-01-02")
	}
	if s, ok := v.(string); ok && s != "" {
		return s[:min(10, len(s))]
	}
	return ""
}

func unixInt(v any) int64 {
	switch t := v.(type) {
	case float64:
		n := int64(t)
		if n > 1e12 {
			n /= 1000
		}
		return n
	case int64:
		return t
	case int:
		return int64(t)
	case json.Number:
		n, _ := t.Int64()
		return n
	default:
		return 0
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// parseRetryAfter parses the Retry-After header (delta-seconds form).
func parseRetryAfter(v string) time.Duration {
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return 0
}

// reattach rebuilds the response with the peeked head re-attached.
func reattach(head []byte, rest io.ReadCloser) io.ReadCloser {
	return struct {
		io.Reader
		io.Closer
	}{io.MultiReader(strings.NewReader(string(head)), rest), rest}
}

// ctxGone reports a canceled/expired request context: the client hung up,
// nothing downstream can succeed — never rotate, never strike.
func ctxGone(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// PostMessages sends one chat request: up to LOAD_SWAPS+1 egress attempts
// (sticky proxy, rotating on load verdicts and dead proxies), a fresh
// single-use captcha param per attempt (minted direct from this host).
func (c *Client) PostMessages(ctx context.Context, jwt, deviceMid string, body []byte, extra http.Header) (*http.Response, error) {
	loadSwaps := c.Cfg.LoadSwaps
	var exclude *url.URL
	busy := 0      // 3010 admission-limit busy-retries (same egress)
	raRetries := 0 // plain-429 Retry-After in-place retries
	for swap := 0; swap <= loadSwaps; swap++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	retry:
		var proxyURL *url.URL
		if c.ProfileForAccount != nil && swap == 0 {
			proxyURL = c.ProfileForAccount(deviceMid)
		}
		if proxyURL == nil && c.Sticky != nil {
			if swap == 0 {
				proxyURL = c.Sticky.Next()
			} else {
				proxyURL = c.Sticky.Rotate(exclude)
			}
			if proxyURL == nil && swap > 0 {
				break // nothing left to rotate to
			}
			// swap 0 with an empty pool: nil proxy = direct egress
			exclude = proxyURL
		} else if swap > 0 {
			break // no pool: single direct attempt
		}
		egress := "direct"
		if proxyURL != nil {
			egress = proxyURL.String()
		}

		// Fresh single-use param per request; the mint runs direct from
		// this host (go_eng parity), the chat rides the sticky egress.
		verify := ""
		verifyRegion := ""
		if !c.Cfg.CaptchaOff && c.Captcha != nil {
			p, region, err := c.Captcha.Solve(ctx)
			if err != nil {
				if ctxGone(err) || ctx.Err() != nil {
					return nil, ctx.Err() // client hung up — no rotation, no strike
				}
				// solver failure is its own retry domain (pe-stalls etc.) —
				// rotate the egress and let the next swap try again (the
				// mint itself is always direct; the egress is chat-only)
				log.Printf("[egress] mint failed (chat egress %s) — rotating (%d/%d): %v", egress, swap+1, loadSwaps, err)
				continue
			}
			verify = p
			verifyRegion = region
		}

		target, err := url.Parse(config.OriginBaseURL + messagesPath)
		if err != nil {
			return nil, err
		}
		headers := chatRequestHeaders(jwt, config.ClientVersion, verify, verifyRegion, deviceMid, extra)
		log.Printf("[egress] chat via %s (mint direct, swap %d)", egress, swap)
		resp, err := sendChat(ctx, proxyURL, target, headers, body, 15*time.Second)
		if err != nil {
			if ctxGone(err) || ctx.Err() != nil {
				return nil, ctx.Err() // client hung up — no rotation, no strike
			}
			// connect-level failure (dead proxy, refused CONNECT): rotate
			log.Printf("[egress] connect failed via %s — rotating (%d/%d)", egress, swap+1, loadSwaps)
			continue
		}
		// load verdict (manager LOAD_CODES semantics): the STATUS comes from
		// resp.StatusCode, the business code from the body JSON — 3012 (edge)
		// and 3007 (app captcha check) are different problems.
		head, rest, perr := PeekBody(resp, 256)

		// plain 429 with Retry-After: honor it (cap 120s), retry in place up
		// to 5 times on the SAME egress
		if perr == nil && resp.StatusCode == http.StatusTooManyRequests &&
			!IsQuotaFamily429(resp.StatusCode, head) && !IsAdmissionLimit(resp.StatusCode, head) {
			if ra := parseRetryAfter(resp.Header.Get("Retry-After")); ra > 0 && raRetries < 5 {
				raRetries++
				if ra > 120*time.Second {
					ra = 120 * time.Second
				}
				resp.Body.Close()
				log.Printf("[egress] 429 via %s — Retry-After %s (%d/5)", egress, ra, raRetries)
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(ra):
				}
				goto retry
			}
		}

		// 3010 admission limit: the account stays usable — busy-retry 1s/2s
		// on the SAME egress (the official client's cadence)
		if perr == nil && IsAdmissionLimit(resp.StatusCode, head) && busy < 2 {
			resp.Body.Close()
			busy++
			log.Printf("[egress] 3010 admission limit via %s — busy-retry in %ds (%d/2)", egress, busy, busy)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(busy) * time.Second):
			}
			goto retry
		}

		// quota-family 429: the account's model is exhausted — hand the
		// response up; the gateway marks the model dead and switches
		// accounts (no egress rotation helps here)
		if perr == nil && IsQuotaFamily429(resp.StatusCode, head) {
			resp.Body = reattach(head, rest)
			log.Printf("[egress] quota-family 429 (code %d) via %s — account switch", originJSONCode(head), egress)
			return resp, nil
		}

		if perr == nil && IsLoadVerdict(resp.StatusCode, head) && swap < loadSwaps {
			resp.Body.Close()
			log.Printf("[egress] load verdict (HTTP %d code %d) via %s — retrying (%d/%d)", resp.StatusCode, originJSONCode(head), egress, swap+1, loadSwaps)
			// IDE cadence: a short pause keeps us from hammering mid-rotation.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		}
		resp.Body = reattach(head, rest)
		return resp, nil
	}
	return nil, fmt.Errorf("no egress accepted the request (load-blocked)")
}
