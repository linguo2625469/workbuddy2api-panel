package upstream

import "testing"

func TestParseGlobalModelInfosSupportsEnvelopeVariants(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"data models", `{"code":0,"data":{"models":[{"id":"global-a","maxInputTokens":65536}]}}`, "global-a"},
		{"data items", `{"code":0,"data":{"items":[{"modelId":"global-b","contextWindow":131072}]}}`, "global-b"},
		{"top level models", `{"models":[{"model":"global-c","maxTokens":4096}]}`, "global-c"},
		{"data string list", `{"code":0,"data":["global-d"]}`, "global-d"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseGlobalModelInfos([]byte(tc.raw))
			if err != nil {
				t.Fatalf("parseGlobalModelInfos() error = %v", err)
			}
			if len(got) != 1 || got[0].ID != tc.want {
				t.Fatalf("models = %#v, want one model %q", got, tc.want)
			}
		})
	}
}

func TestParseGlobalModelInfosRejectsNonZeroCode(t *testing.T) {
	if _, err := parseGlobalModelInfos([]byte(`{"code":14017,"data":{"models":[]}}`)); err == nil {
		t.Fatal("expected non-zero upstream code to fail")
	}
}

// TestParseGlobalModelInfosKeepsReasoning 思考档位与窗口元数据不能被兼容解析丢掉：
// 面板「模型与档位」与出站 effort 降级都依赖这些字段。
func TestParseGlobalModelInfosKeepsReasoning(t *testing.T) {
	raw := `{"code":0,"data":{"models":[{
		"id":"deepseek-v4.1-flash",
		"name":"Deepseek-V4.1-Flash",
		"maxInputTokens":1000000,
		"maxOutputTokens":393216,
		"maxAllowedSize":1000000,
		"credits":"x0.03",
		"supportsReasoning":true,
		"supportsImages":true,
		"reasoning":{"defaultEffort":"high","canDisableThinking":true,"supportedEfforts":["low","high","max"]}
	}]}}`
	got, err := parseGlobalModelInfos([]byte(raw))
	if err != nil {
		t.Fatalf("parseGlobalModelInfos() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d models, want 1", len(got))
	}
	m := got[0]
	if m.ContextWindow != 1000000 || m.MaxTokens != 393216 || m.MaxAllowedSize != 1000000 {
		t.Errorf("窗口元数据丢失: %+v", m)
	}
	if m.DefaultEffort != "high" || !m.CanDisableThinking || !m.SupportsReasoning || !m.SupportsImages {
		t.Errorf("能力字段丢失: %+v", m)
	}
	if len(m.Efforts) != 3 || m.Efforts[0] != "low" || m.Efforts[2] != "max" {
		t.Errorf("supportedEfforts = %v, want [low high max]", m.Efforts)
	}
	if m.Credits != "" {
		t.Errorf("Credits 不应进入 global 路径，got %q", m.Credits)
	}
}

// TestParseGlobalModelInfosReasoningLegacyKey 老模型只回 effort 键时要能兜住默认档。
func TestParseGlobalModelInfosReasoningLegacyKey(t *testing.T) {
	got, err := parseGlobalModelInfos([]byte(`{"code":0,"data":{"models":[{"id":"legacy","reasoning":{"effort":"max"}}]}}`))
	if err != nil || len(got) != 1 {
		t.Fatalf("parseGlobalModelInfos() = (%v, %v)", got, err)
	}
	if got[0].DefaultEffort != "max" {
		t.Errorf("DefaultEffort = %q, want max", got[0].DefaultEffort)
	}
}
