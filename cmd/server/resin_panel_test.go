package main

import "testing"

func TestApplyResinPanelIntentPreservesSecretWhenEnabled(t *testing.T) {
	current := map[string]any{"resin": map[string]any{"url": "http://resin/secret", "platform": "old"}}
	incoming := map[string]any{"resin": map[string]any{"enabled": true, "platform": "workbuddy2api"}}
	if err := applyResinPanelIntent(current, incoming); err != nil {
		t.Fatal(err)
	}
	resin := incoming["resin"].(map[string]any)
	if _, exists := resin["enabled"]; exists {
		t.Fatal("panel-only enabled flag must not persist")
	}
	if _, exists := resin["url"]; exists {
		t.Fatal("blank field must preserve the existing URL through merge")
	}
}

func TestApplyResinPanelIntentClearsSecretWhenDisabled(t *testing.T) {
	incoming := map[string]any{"resin": map[string]any{"enabled": false}}
	if err := applyResinPanelIntent(map[string]any{}, incoming); err != nil {
		t.Fatal(err)
	}
	if got := incoming["resin"].(map[string]any)["url"]; got != "" {
		t.Fatalf("url = %v, want empty", got)
	}
}

func TestApplyResinPanelIntentRejectsNewEnabledWithoutURL(t *testing.T) {
	incoming := map[string]any{"resin": map[string]any{"enabled": true}}
	if err := applyResinPanelIntent(map[string]any{}, incoming); err == nil {
		t.Fatal("want error for new enabled Resin without URL")
	}
}
