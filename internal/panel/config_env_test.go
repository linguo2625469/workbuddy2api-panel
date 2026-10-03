package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConfigAPIReportsEnvironmentManagedKey(t *testing.T) {
	t.Setenv("WB2A_API_KEY", "environment-key")
	p := New(Config{APIKey: "environment-key", LoadConfig: func() (any, error) { return map[string]any{"api_key": "environment-key"}, nil }, SaveConfig: func([]byte) ([]string, error) { return nil, nil }})
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		req := httptest.NewRequest(method, "/panel/api/config", strings.NewReader(`{"api_key":"other"}`))
		req.Header.Set("Authorization", "Bearer environment-key")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		var response map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if rec.Code != 200 || response["api_key_env_managed"] != true {
			t.Fatalf("method=%s status=%d response=%v", method, rec.Code, response)
		}
	}
}
