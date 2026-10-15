// Package captcha — minting Aliyun captcha verify params.
//
// The only backend is the node solver (nodesolver.go): the happy-dom
// page at the real zcode.z.ai origin, where the Aliyun FeiLin/pe.* device
// VM runs to completion — the mint that produces full-strength params.
// The mint egress is always this host, direct (go_eng parity).
package captcha

import (
	"context"
	"encoding/base64"
	"encoding/json"
)

// Solver mints a fresh single-use verify param per chat request.
type Solver interface {
	// Solve mints a fresh verify param AND the region it was minted for —
	// the chat request must declare the SAME region (X-Aliyun-Captcha-
	// Verify-Region) — the region rides with the param. The mint
	// egress is this host, direct (go_eng parity).
	Solve(ctx context.Context) (param, region string, err error)
	Close()
}

// Aliyun captcha scene (from zcode client/configs and IDE HAR captures).
const (
	SceneID     = "11xygtvd"
	InitSceneID = "11xygtvd"
)

// ReadyCaptchaHeader validates a captcha verify param: base64 JSON with
// certifyId + securityToken + isSign. A securityToken shorter than 50 chars
// is the SDK's degraded no-device result —
// upstream answers it 3007, so it never counts as ready.
func ReadyCaptchaHeader(s string) bool {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return false
	}
	var p struct {
		SecurityToken string `json:"securityToken"`
		CertifyID     string `json:"certifyId"`
		IsSign        bool   `json:"isSign"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return false
	}
	return len(p.SecurityToken) >= 50 && p.CertifyID != "" && p.IsSign
}
