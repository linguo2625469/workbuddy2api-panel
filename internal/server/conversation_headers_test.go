package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestChatWithoutConversationKeyPassesChannelValidation(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			status, body := http.StatusOK, sseOK
			for _, name := range []string{"X-Conversation-ID", "X-Conversation-Request-ID", "X-Conversation-Message-ID"} {
				if r.Header.Get(name) == "" {
					status, body = http.StatusBadRequest, `{"code":11128,"msg":"conversation id missing"}`
				}
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})},
		ChatBaseGlobal: "https://fake.example", GlobalEnabled: true,
	}
	a, err := auth.Parse([]byte(`{"uid":"global-test","accessToken":"test-token","realm":"global","expiresAt":9999999999}`))
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: testPoolWith(a), Upstream: up, GlobalEnabled: true})
	// A user identifier is not a conversation key; this also disables the
	// session-key derivation used for otherwise unlabelled text conversations.
	body := `{"model":"global:deepseek-v4.1-flash","stream":true,"metadata":{"user_id":"same-user"},"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("unlabelled conversation rejected: status=%d body=%s", rec.Code, rec.Body)
	}
}
