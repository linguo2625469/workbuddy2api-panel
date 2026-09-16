package server

import "testing"

func TestResolveModelKeepsGlobalRealmPrefix(t *testing.T) {
	realm, bare := ResolveModel("global:gpt-5.6-luna")
	if realm != "global" || bare != "gpt-5.6-luna" {
		t.Fatalf("ResolveModel() = (%q, %q), want (global, gpt-5.6-luna)", realm, bare)
	}
}

func TestResolveModelBareNameRemainsCN(t *testing.T) {
	realm, bare := ResolveModel("gpt-5.6-luna")
	if realm != "cn" || bare != "gpt-5.6-luna" {
		t.Fatalf("ResolveModel() = (%q, %q), want (cn, gpt-5.6-luna)", realm, bare)
	}
}
