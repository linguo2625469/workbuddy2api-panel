package upstream

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// resinAccountHeader is read by Resin and identifies the sticky lease owner.
// UID values are stable before any request is issued, so retries and scheduled
// tasks keep the same egress identity as interactive chat traffic.
const resinAccountHeader = "X-Resin-Account"

type resinRoundTripper struct {
	next     http.RoundTripper
	base     *url.URL
	platform string
}

// ConfigureResin enables Resin reverse-proxy routing for requests that carry a
// per-account identity. Empty URL leaves the existing transport untouched.
func (c *Client) ConfigureResin(rawURL, platform string) error {
	if c == nil || c.HTTP == nil {
		return fmt.Errorf("upstream client is not initialized")
	}
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil
	}
	base, err := url.Parse(rawURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return fmt.Errorf("resin.url must be an absolute http(s) URL")
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return fmt.Errorf("resin.url scheme must be http or https")
	}
	platform = strings.TrimSpace(platform)
	if platform == "" || strings.Contains(platform, "/") {
		return fmt.Errorf("resin.platform must be a non-empty path segment")
	}
	wrap := func(rt http.RoundTripper) http.RoundTripper {
		rt = unwrapResinRoundTripper(rt)
		if rt == nil {
			rt = http.DefaultTransport
		}
		return &resinRoundTripper{next: rt, base: base, platform: platform}
	}
	c.HTTP.Transport = wrap(c.HTTP.Transport)
	if c.ChatHTTP != nil {
		c.ChatHTTP.Transport = wrap(c.ChatHTTP.Transport)
	}
	c.resinConfigured = true
	return nil
}

func unwrapResinRoundTripper(rt http.RoundTripper) http.RoundTripper {
	for {
		rrt, ok := rt.(*resinRoundTripper)
		if !ok {
			return rt
		}
		rt = rrt.next
	}
}

func unwrapHTTPTransport(rt http.RoundTripper) *http.Transport {
	rt = unwrapResinRoundTripper(rt)
	tr, _ := rt.(*http.Transport)
	return tr
}

func (c *Client) resinEnabled() bool {
	return c != nil && c.resinConfigured
}

// CloseIdleConnections forwards idle pool cleanup through the Resin wrapper.
func (r *resinRoundTripper) CloseIdleConnections() {
	roundTripCloseIdle(r.next)
}

func requestHasAccountIdentity(req *http.Request) bool {
	return req.Header.Get("Authorization") != "" ||
		req.Header.Get("X-User-Id") != "" ||
		req.Header.Get("X-Refresh-Token") != ""
}

func (r *resinRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// Unauthenticated metadata fetches (e.g. models.dev) have no account identity
	// and go direct. Authenticated requests missing X-Resin-Account fail closed
	// so misconfigured/unmarked account traffic never leaks the host IP.
	if req.Header.Get(resinAccountHeader) == "" {
		if requestHasAccountIdentity(req) {
			return nil, fmt.Errorf("resin proxy: refusing direct egress for account request missing %s", resinAccountHeader)
		}
		return r.next.RoundTrip(req)
	}
	original := req.URL
	if original == nil || original.Scheme == "" || original.Host == "" {
		return nil, fmt.Errorf("resin proxy requires an absolute upstream URL")
	}
	out := req.Clone(req.Context())
	out.Header = req.Header.Clone()
	targetPath := original.EscapedPath()
	if targetPath == "" {
		targetPath = "/"
	}
	proxyURL := *r.base
	proxyURL.Path = strings.TrimRight(r.base.Path, "/") + "/" + r.platform + "/" + original.Scheme + "/" + original.Host + targetPath
	proxyURL.RawPath = ""
	proxyURL.RawQuery = original.RawQuery
	out.URL = &proxyURL
	// Host must identify Resin for HTTP/1.1 routing; the original target is
	// encoded in the reverse-proxy path above.
	out.Host = ""
	return r.next.RoundTrip(out)
}
