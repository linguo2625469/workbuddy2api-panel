package upstream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// The international chat endpoint rejects an incomplete conversation header trio
// with 400/code 11128, including calls made with a zero-value ChatMeta.
func TestChatStreamConversationChannelHeaders(t *testing.T) {
	for _, realm := range []string{"cn", "global"} {
		for _, tc := range []struct {
			name string
			meta ChatMeta
		}{
			{name: "zero metadata"},
			{name: "request ID only", meta: ChatMeta{ConversationRequestID: "existing-turn"}},
			{name: "explicit conversation", meta: ChatMeta{ConversationID: "existing-dialog", ConversationRequestID: "existing-turn"}},
		} {
			t.Run(realm+"/"+tc.name, func(t *testing.T) {
				headers := make(chan http.Header, 2)
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					headers <- r.Header.Clone()
					for _, name := range []string{"X-Conversation-ID", "X-Conversation-Request-ID", "X-Conversation-Message-ID"} {
						if r.Header.Get(name) == "" {
							w.WriteHeader(http.StatusBadRequest)
							io.WriteString(w, `{"code":11128,"msg":"conversation id missing"}`)
							return
						}
					}
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"pong\"}}]}\n\ndata: [DONE]\n\n")
				}))
				defer srv.Close()
				c := New()
				c.ChatBaseCN, c.ChatBaseGlobal = srv.URL, srv.URL
				a, err := auth.Parse([]byte(`{"uid":"test","accessToken":"test-token","realm":"` + realm + `"}`))
				if err != nil {
					t.Fatal(err)
				}
				var previousID string
				for i := 0; i < 2; i++ {
					rc, status, body, err := c.ChatStream(a, []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`), "", tc.meta)
					if err != nil || status != http.StatusOK {
						t.Fatalf("chat: status=%d body=%s err=%v", status, body, err)
					}
					stream, err := io.ReadAll(rc)
					rc.Close()
					if err != nil || !strings.Contains(string(stream), "pong") || !strings.Contains(string(stream), "[DONE]") {
						t.Fatalf("incomplete chat stream: %s err=%v", stream, err)
					}
					h := <-headers
					id := h.Get("X-Conversation-ID")
					if tc.meta.ConversationID != "" {
						if id != tc.meta.ConversationID {
							t.Fatalf("explicit conversation ID changed: %q", id)
						}
					} else if len(id) != 32 || !validTraceID(id) || id == previousID {
						t.Fatalf("fallback ID must be fresh 32 hex: %q (previous %q)", id, previousID)
					}
					if tc.meta.ConversationRequestID != "" && h.Get("X-Conversation-Request-ID") != tc.meta.ConversationRequestID {
						t.Fatal("explicit request ID changed")
					}
					if h.Get("X-Conversation-Message-ID") != h.Get("X-Request-ID") {
						t.Fatal("message and request IDs lost their existing relationship")
					}
					previousID = id
				}
			})
		}
	}
}
