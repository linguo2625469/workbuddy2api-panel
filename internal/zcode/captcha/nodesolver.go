package captcha

// nodesolver.go — mint verify params by spawning the project's node solver
// (embedded captcha_node/, extracted to data/captcha_node on first use —
// see embed.go; happy-dom window at the REAL https://zcode.z.ai/ origin:
// the Aliyun FeiLin/pe.* device VM runs to completion, per-request
// referer/origin/sec-ch-ua headers, primed zcode cookies — the mint that
// passes the edge).
//
// Protocol (one process = one solve):
//
//	node solver.js <scene> <region> <prefix>
//	  → stdout line "VERIFY_PARAM=<param>", exit 0
//	  exit 2 = pe-stall/timeout — the solver evicted the stalled bundle;
//	  an immediate retry fetches a fresh one (observed: stall, then pass).
//
// IP binding: NONE — the mint runs direct from this host; only the chat
// request rides the egress proxy. The child env is scrubbed of the
// machine's mitm/proxy vars so the solver's network is truly direct.
//
// ZCODE_NODE_SOLVER_CMD     — full spawn override (test hook; the cwd
//                             stays the caller's)

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/zcode/config"
	"github.com/linguo2625469/workbuddy2api-panel/internal/zcode/util"
)

// NodeSolver mints verify params via the happy-dom node solver.
type NodeSolver struct {
	cfg config.Config
	// mintSem serializes ALL mints (pool loop, empty-stock fallbacks,
	// restocks, claims): the solver processes share the on-disk
	// ~/.zcode-captcha-cdn-cache and are designed to run ONE at a time —
	// concurrent processes race on cache writes/evictions (exit-1 storms).
	mintSem chan struct{}
}

func NewNodeSolver(cfg config.Config) *NodeSolver {
	return &NodeSolver{cfg: cfg, mintSem: make(chan struct{}, 1)}
}

func (n *NodeSolver) nodePath() string { return "node" }

// Solve spawns the solver once per attempt (retries cover pe-stalls).
// Concurrent callers queue here — one node process at a time.
func (n *NodeSolver) Solve(ctx context.Context) (string, string, error) {
	select {
	case n.mintSem <- struct{}{}:
		defer func() { <-n.mintSem }()
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
	cc := fetchCaptchaCfg(ctx)
	if !cc.Enabled {
		return "", "", fmt.Errorf("%w: captcha disabled by origin config", ErrCaptchaUnavailable)
	}

	retries := n.cfg.CaptchaRetries
	if retries < 2 {
		retries = 2 // pe-stall on the first run is normal lifecycle, not failure
	}
	var lastErr error
	for attempt := 1; attempt <= retries; attempt++ {
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		param, err := n.solveOnce(ctx, cc)
		if err == nil {
			log.Printf("[captcha] node solver minted param %s (%d bytes, region %s, attempt %d/%d)", util.TruncateToken(param), len(param), cc.Region, attempt, retries)
			// the region rides with the token: the chat request must declare
			// the region the param was minted for
			return param, cc.Region, nil
		}
		lastErr = err
		log.Printf("[captcha] node solver attempt %d/%d failed: %v", attempt, retries, err)
	}
	return "", "", fmt.Errorf("%w: %v", ErrCaptchaUnavailable, lastErr)
}

// childEnv strips the machine's mitm/proxy/CA env so the solver process
// talks to zcode.z.ai and aliyuncs.com directly from this host.
func childEnv() []string {
	drop := map[string]bool{
		"http_proxy": true, "https_proxy": true, "all_proxy": true,
		"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true,
		"SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
		"REQUESTS_CA_BUNDLE": true, "CURL_CA_BUNDLE": true,
		"NODE_EXTRA_CA_CERTS": true, "NODE_TLS_REJECT_UNAUTHORIZED": true,
	}
	env := os.Environ()[:0]
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 && !drop[kv[:i]] {
			env = append(env, kv)
		}
	}
	return env
}

func (n *NodeSolver) solveOnce(ctx context.Context, cc captchaCfg) (string, error) {
	var args []string
	dir := ""
	if override := strings.TrimSpace(os.Getenv("ZCODE_NODE_SOLVER_CMD")); override != "" {
		// test hook: full spawn override; the cwd stays the caller's
		args = strings.Fields(override)
	} else {
		var err error
		if dir, err = ensureSolverDir(); err != nil {
			return "", fmt.Errorf("solver extract: %w", err)
		}
		args = []string{n.nodePath(), "solver.js", cc.SceneID, cc.Region, cc.Prefix}
	}
	cctx, cancel := context.WithTimeout(ctx, n.cfg.CaptchaTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, args[0], args[1:]...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = childEnv()
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		tail := strings.TrimSpace(errb.String())
		if len(tail) > 200 {
			tail = tail[len(tail)-200:]
		}
		return "", fmt.Errorf("solver exit: %v %s", err, tail)
	}
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		line := sc.Text()
		if p := strings.TrimSpace(strings.TrimPrefix(line, "VERIFY_PARAM=")); strings.HasPrefix(line, "VERIFY_PARAM=") && p != "" {
			if !ReadyCaptchaHeader(p) {
				return "", fmt.Errorf("solver returned a degraded param (no securityToken)")
			}
			return p, nil
		}
	}
	return "", fmt.Errorf("no VERIFY_PARAM in solver output")
}

func (n *NodeSolver) Close() {}

// ensure NodeSolver satisfies the solver interface (compile-time check)
var _ Solver = (*NodeSolver)(nil)
