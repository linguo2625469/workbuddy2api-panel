// warm.go — account warming for freshly added JWTs (site-registered, no
// manager identity): the pool-admission flow
//
//  1. POST /api/v1/event/report ×2 (app_launch, app_daily_active) — NO
//     Authorization; the 16-field body is keyed by device_mid (upstream
//     dedups daily-active by device_mid+date).
//  2. GET  billing/preview → POST billing/claim for EVERY plan the origin
//     itself offers (the offer list is the filter — no name heuristics),
//     priority first, with a freshly minted captcha param; 3007 retries
//     once with a re-minted param, 1003 already-claimed is a success.
//
// No standalone balance call in the sequence; the dashboard quota refresh
// picks the granted state up.
// Events and billing ride the account egress (with a direct retry on
// network failure); the mint is always direct.
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/zcode/config"
	"github.com/linguo2625469/workbuddy2api-panel/internal/zcode/store"
	"github.com/linguo2625469/workbuddy2api-panel/internal/zcode/util"
)

// WarmReport is the per-account warming outcome (empty step = ok).
type WarmReport struct {
	Account string `json:"account"`
	Events  string `json:"events,omitempty"`
	Claim   string `json:"claim,omitempty"`
	Plans   string `json:"plans,omitempty"`
}

// BuildActivationEvent — the official sendReport 16-field body, field by field. Language/timezone = a real region pair
// (fingerprint.py _LOCALES), same pair the billing headers declare.
func BuildActivationEvent(element, userID, deviceMid, appVersion string) map[string]any {
	return map[string]any{
		"event_id":           util.GenerateUUID(),
		"client_timezone":    "America/New_York",
		"client_language":    "en-US",
		"element_name":       element,
		"event_region":       "app",
		"event_type":         "view",
		"event_text":         "",
		"event_extra_detail": map[string]any{},
		"user_id":            userID,
		"screen_resolution":  "1920x1080",
		"app_version":        appVersion,
		"device_os_category": "windows",
		"device_os_version":  "10.0.26200",
		"device_mid":         deviceMid,
		"mac_id":             "",
		"marketing_params":   "{}",
	}
}

var activationElements = []string{"app_launch", "app_daily_active"}

func (c *Client) postEvent(ctx context.Context, element, userID, mid string) error {
	body, _ := json.Marshal(BuildActivationEvent(element, userID, mid, config.BillingClientVersion))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		config.OriginBaseURL+"/api/v1/event/report", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.egressDo(mid, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("event %s HTTP %d", element, resp.StatusCode)
	}
	return nil
}

// ClaimOfferedPlans: preview returns the
// plans the ORIGIN itself offers this account — claim every one of them,
// priority first (the offer list IS the filter, no name heuristics).
// Returns the claimed plan names and a joined problem string (both may be
// empty when nothing is offered). The manual dashboard Claim runs exactly
// this (no activation events — those belong to the warm admission flow).
func (c *Client) ClaimOfferedPlans(ctx context.Context, jwt, mid string) (string, string) {
	base := config.OriginBaseURL + "/api/v1/zcode-plan"
	status, raw, err := c.getJSON(ctx, http.MethodGet, base+"/billing/preview?app_version="+config.BillingClientVersion+"&platform=darwin-arm64", jwt, mid, nil, nil)
	if err != nil {
		return "", "preview: " + err.Error()
	}
	if status != 200 {
		return "", fmt.Sprintf("preview HTTP %d: %s", status, BodySnip(raw, 120))
	}
	var pw struct {
		Code int `json:"code"`
		Data struct {
			Plans []struct {
				PlanID   string `json:"plan_id"`
				Name     string `json:"name"`
				Priority int    `json:"priority"`
			} `json:"plans"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &pw) != nil || pw.Code != 0 {
		return "", fmt.Sprintf("preview code %d", pw.Code)
	}
	sort.SliceStable(pw.Data.Plans, func(i, j int) bool { return pw.Data.Plans[i].Priority > pw.Data.Plans[j].Priority })
	var claimed, problems []string
	for _, p := range pw.Data.Plans {
		if p.PlanID == "" {
			continue
		}
		if err := c.claimPlan(ctx, base, jwt, mid, p.PlanID); err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if p.Name == "" {
			p.Name = p.PlanID
		}
		claimed = append(claimed, p.Name)
	}
	return strings.Join(claimed, ", "), strings.Join(problems, "; ")
}

// claimPlan: captcha → billing/claim; on 3007
// (captcha rejected) retry once with a freshly minted param. 1003
// already-claimed is a success for warming.
func (c *Client) claimPlan(ctx context.Context, base, jwt, mid, planID string) error {
	body, _ := json.Marshal(map[string]string{"plan_id": planID})
	for attempt := 1; attempt <= 2; attempt++ {
		extra := http.Header{}
		if c.Captcha != nil {
			param, region, err := c.Captcha.Solve(ctx)
			if err != nil {
				return fmt.Errorf("claim %s mint: %w", planID, err)
			}
			if param != "" {
				extra.Set("X-Aliyun-Captcha-Verify-Param", param)
				if region != "" {
					extra.Set("X-Aliyun-Captcha-Verify-Region", region)
				}
			}
		}
		st, raw, err := c.getJSON(ctx, http.MethodPost, base+"/billing/claim", jwt, mid, body, extra)
		if err != nil {
			return fmt.Errorf("claim %s: %w", planID, err)
		}
		if st != 200 && st != 401 && st != 403 {
			return fmt.Errorf("claim %s HTTP %d: %s", planID, st, BodySnip(raw, 120))
		}
		var cw struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		_ = json.Unmarshal(raw, &cw)
		switch {
		case cw.Code == 0 || cw.Code == 1003:
			return nil
		case cw.Code == 3007 && attempt == 1:
			continue // fresh captcha retry
		default:
			return fmt.Errorf("claim %s code %d %s", planID, cw.Code, cw.Msg)
		}
	}
	return fmt.Errorf("claim %s: captcha rejected twice", planID)
}

func (c *Client) getJSON(ctx context.Context, method, url, jwt, mid string, body []byte, extra http.Header) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return 0, nil, err
	}
	h := billingHeaders(jwt, mid, config.BillingClientVersion)
	for k, vs := range extra {
		for _, v := range vs {
			h.Set(k, v)
		}
	}
	req.Header = h
	resp, err := c.egressDo(mid, req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, err
}

// WarmAccount runs the warming sequence for one account (pool admission):
// authless activation events → preview → claim of
// every offered plan. No standalone balance call — the dashboard quota
// refresh picks the granted state up afterwards.
func (c *Client) WarmAccount(ctx context.Context, jwt, mid string) WarmReport {
	rep := WarmReport{Account: mid}
	userID := store.JWTSub(jwt)
	if userID == "" {
		rep.Events = "JWT has no user_id/sub — events skipped"
	} else {
		for _, el := range activationElements {
			if err := c.postEvent(ctx, el, userID, mid); err != nil {
				rep.Events = err.Error()
				break
			}
		}
	}
	if rep.Events == "" {
		claimed, problems := c.ClaimOfferedPlans(ctx, jwt, mid)
		switch {
		case claimed != "":
			if problems != "" {
				rep.Claim = problems
			}
			rep.Plans = "warmed (events ok; claimed: " + claimed + ")"
		case problems != "":
			rep.Claim = problems
			rep.Plans = "warmed (events ok; claim: " + problems + ")"
		default:
			rep.Plans = "warmed (events ok; no offered plans)"
		}
	}
	return rep
}
