package panel

import "testing"

func TestConfigViewRedactsResinURL(t *testing.T) {
	view, err := configView(map[string]any{
		"resin": map[string]any{
			"url":      "http://resin:2260/secret-token",
			"platform": "workbuddy2api",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	resin := view["resin"].(map[string]any)
	if _, exists := resin["url"]; exists {
		t.Fatal("resin URL leaked into panel config response")
	}
	if enabled, _ := resin["enabled"].(bool); !enabled {
		t.Fatalf("enabled = %v, want true", resin["enabled"])
	}
	if got := resin["platform"]; got != "workbuddy2api" {
		t.Fatalf("platform = %v", got)
	}
}

func TestConfigViewMarksEmptyResinURLDisabled(t *testing.T) {
	view, err := configView(map[string]any{"resin": map[string]any{"url": ""}})
	if err != nil {
		t.Fatal(err)
	}
	if view["resin"].(map[string]any)["enabled"] != false {
		t.Fatal("empty Resin URL must be disabled")
	}
}
