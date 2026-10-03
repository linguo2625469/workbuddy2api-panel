package httpauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

func TestNewKeyringSkipsDisabledAndEmpty(t *testing.T) {
	if kr := NewKeyring(nil); kr != nil {
		t.Fatalf("empty specs should yield nil keyring, got %v", kr)
	}
	kr := NewKeyring([]KeySpec{
		{Key: "", Enabled: true},
		{Key: "sk-a", Enabled: false},
	})
	if kr != nil {
		t.Fatalf("all-disabled should yield nil keyring, got %v", kr)
	}
	kr = NewKeyring([]KeySpec{{Key: "sk-a", Enabled: true}, {Key: "sk-b", Enabled: false}})
	if kr == nil || kr.Len() != 1 {
		t.Fatalf("keyring = %v (len %d), want 1", kr, kr.Len())
	}
}

func TestKeyringMatch(t *testing.T) {
	kr := NewKeyring([]KeySpec{
		{Key: "sk-codex", Name: "codex", Enabled: true},
		{Key: "sk-phone", Name: "phone", Enabled: true},
	})
	cases := []struct {
		header string
		want   string
		ok     bool
	}{
		{"Bearer sk-codex", "codex", true},
		{"Bearer sk-phone", "phone", true},
		{"Bearer sk-nope", "", false},
		{"", "", false},
		{"sk-codex", "", false}, // 缺 Bearer 前缀
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		if c.header != "" {
			req.Header.Set("Authorization", c.header)
		}
		spec, ok := kr.Match(req)
		if ok != c.ok || spec.Name != c.want {
			t.Errorf("Match(%q) = (%q,%v), want (%q,%v)", c.header, spec.Name, ok, c.want, c.ok)
		}
	}
}

func TestMatchModel(t *testing.T) {
	cases := []struct {
		patterns []string
		model    string
		want     bool
	}{
		{nil, "anything", true},                                   // 空 = 不限制
		{[]string{}, "anything", true},                            // 同上
		{[]string{"glm-5.2"}, "glm-5.2", true},                    // 精确
		{[]string{"glm-5.2"}, "glm-5.3", false},                   // 不命中
		{[]string{"deepseek*"}, "deepseek-v4.1-flash", true},      // 前缀通配
		{[]string{"*"}, "anything", true},                         // 全放行
		{[]string{"gpt-6-astr?"}, "gpt-6-astra", true},            // 单字符通配
		{[]string{"GPT-6-ASTRA"}, "gpt-6-astra", true},            // 大小写不敏感
		{[]string{" glm-5.2 "}, "glm-5.2", true},                  // 去空白
		{[]string{"glm-5.2", "kimi-k3"}, "kimi-k3", true},         // 多模式任一命中
		{[]string{"glm-5.2", "kimi-k3"}, "deepseek-v4.1", false},  // 多模式全不中
		{[]string{"glm-5.2"}, "", false},                          // 空模型名
	}
	for _, c := range cases {
		if got := MatchModel(c.patterns, c.model); got != c.want {
			t.Errorf("MatchModel(%v, %q) = %v, want %v", c.patterns, c.model, got, c.want)
		}
	}
}

func TestGlobMatchEdgeCases(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"a*b*c", "aXXbYYc", true},
		{"a*b*c", "aXXbYY", false},
		{"*b*", "ab", true},
		{"a*", "a", true},
		{"*", "", true},
		{"?", "", false},
		{"a?c", "abc", true},
		{"", "", true},
		{"", "x", false},
	}
	for _, c := range cases {
		if got := globMatch(c.pattern, c.s); got != c.want {
			t.Errorf("globMatch(%q,%q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}
