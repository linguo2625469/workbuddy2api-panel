package upstream

// chathttp.go — the chat transport: plain net/http over the stdlib TLS
// stack, HTTP/1.1, with a cached Transport per egress so keep-alive
// connections are reused. go_eng parity: the edge accepts the stock Go
// carrier — all the stealth lives in the mint, none in the transport.

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var chatTransports sync.Map // egress URL ("" = direct) -> *http.Transport

func chatTransport(proxyURL *url.URL) *http.Transport {
	key := ""
	if proxyURL != nil {
		key = proxyURL.String()
	}
	if v, ok := chatTransports.Load(key); ok {
		return v.(*http.Transport)
	}
	t := &http.Transport{
		// custom DialContext without ForceAttemptHTTP2 keeps us on HTTP/1.1
		// (go_eng parity: their identical setup passes the edge)
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
	}
	if proxyURL != nil {
		t.Proxy = http.ProxyURL(proxyURL)
	}
	actual, _ := chatTransports.LoadOrStore(key, t)
	return actual.(*http.Transport)
}

func egressLabel(proxyURL *url.URL) string {
	if proxyURL != nil {
		return proxyURL.String()
	}
	return "direct"
}

// sendChat POSTs the chat request through plain net/http; headers is the
// wire set (casing/order are net/http's own — go_eng parity). The response
// body streams natively.
func sendChat(ctx context.Context, proxyURL, target *url.URL, headers [][2]string, body []byte, _ time.Duration) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.ContentLength = int64(len(body))
	for _, kv := range headers {
		req.Header.Set(kv[0], kv[1])
	}
	client := &http.Client{Transport: chatTransport(proxyURL)}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	// We set accept-encoding ourselves, so net/http will
	// NOT transparently decode — do it here or every consumer (verdict
	// classifier, SSE relay, OpenAI converter) sees gzip bytes.
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		if gr, err := gzip.NewReader(resp.Body); err == nil {
			resp.Header.Del("Content-Encoding")
			resp.Body = struct {
				io.Reader
				io.Closer
			}{gr, resp.Body}
		}
	}
	log.Printf("[tls] net/http h1 via %s", egressLabel(proxyURL))
	return resp, nil
}

// peekBody reads up to n bytes ahead, returning them plus the rest of the
// body for re-attachment (the verdict classifier must see the head without
// consuming the stream).
// PeekBody reads up to n bytes ahead.
func PeekBody(resp *http.Response, n int) (head []byte, rest io.ReadCloser, err error) {
	buf := make([]byte, n)
	got, err := io.ReadFull(resp.Body, buf)
	if err == io.ErrUnexpectedEOF || err == io.EOF {
		err = nil
		buf = buf[:got]
	} else if err != nil {
		return nil, resp.Body, err
	} else {
		buf = buf[:got]
	}
	if len(buf) < n || err == io.EOF {
		return buf, io.NopCloser(bytes.NewReader(nil)), nil
	}
	return buf, resp.Body, nil
}
