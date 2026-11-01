package upstream

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestResinRoutesAccountRequestAndKeepsIdentity(t *testing.T) {
	var gotPath, gotAccount string
	resin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAccount = r.URL.RequestURI(), r.Header.Get(resinAccountHeader)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer resin.Close()
	c := New()
	if err := c.ConfigureResin(resin.URL+"/secret", "workbuddy2api"); err != nil {
		t.Fatal(err)
	}
	a := &auth.Auth{UID: "account-1"}
	req, _ := http.NewRequest(http.MethodGet, "https://copilot.tencent.com/v2/models?x=1", nil)
	c.CommonHeaders(req, a)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotPath != "/secret/workbuddy2api/https/copilot.tencent.com/v2/models?x=1" {
		t.Fatalf("proxy path = %q", gotPath)
	}
	if gotAccount != "account-1" {
		t.Fatalf("resin account = %q", gotAccount)
	}
}

func TestResinLeavesNonAccountRequestDirect(t *testing.T) {
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer direct.Close()
	c := New()
	if err := c.ConfigureResin("http://127.0.0.1:1/secret", "workbuddy2api"); err != nil {
		t.Fatal(err)
	}
	resp, err := c.HTTP.Get(direct.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}
