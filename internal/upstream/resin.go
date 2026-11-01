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
		if rt == nil {
			rt = http.DefaultTransport
		}
		return &resinRoundTripper{next: rt, base: base, platform: platform}
	}
	c.HTTP.Transport = wrap(c.HTTP.Transport)
	if c.ChatHTTP != nil {
		c.ChatHTTP.Transport = wrap(c.ChatHTTP.Transport)
	}
	return nil
}

func (c *Client) resinEnabled() bool {
	if c == nil || c.HTTP == nil {
		return false
	}
	_, ok := c.HTTP.Transport.(*resinRoundTripper)
	return ok
}

func (r *resinRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// Requests without an account identity are intentionally left alone. This
	// preserves unauthenticated metadata fetches while every account operation
	// is explicitly marked by the shared header builders.
	if req.Header.Get(resinAccountHeader) == "" {
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
