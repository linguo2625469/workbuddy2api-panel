// login.go — Z.AI OAuth login (ZaiAuthFlow:
// admin_api login/start|poll): the dashboard starts a flow, the user
// authorizes in their browser, the server polls; on success the Coding
// Plan JWT joins the pool (auto-warmed) and an API key is exchanged as a
// same-account fallback credential.
package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
	"github.com/linguo2625469/workbuddy2api-panel/internal/zcode/config"
)

const loginFlowTTL = 300 * time.Second

type loginFlow struct {
	pollToken string
	created   time.Time
	label     string
}

func (s *Server) oauthBase() string {
	if v := strings.TrimSpace(oauthBaseEnv()); v != "" {
		return v
	}
	return config.OriginBaseURL + "/api/v1"
}

func (s *Server) exchangeOrigin() string {
	if v := strings.TrimSpace(exchangeOriginEnv()); v != "" {
		return v
	}
	return "https://api.z.ai"
}

func (s *Server) loginGC() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, f := range s.loginFlows {
		if time.Since(f.created) > loginFlowTTL {
			delete(s.loginFlows, id)
		}
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// apiLoginStart: POST /api/login/start {"label":"acct-1"}
func (s *Server) apiLoginStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Label string `json:"label"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	label := strings.TrimSpace(body.Label)
	if len(label) > 32 {
		label = label[:32]
	}
	s.loginGC()

	flow := &loginFlow{pollToken: randomHex(32), created: time.Now(), label: label}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost,
		s.oauthBase()+"/oauth/cli/init", bytes.NewReader([]byte(`{"provider":"zai"}`)))
	req.Header.Set("Authorization", "Bearer "+flow.pollToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "login init: "+err.Error())
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		writeJSONError(w, http.StatusBadGateway, fmt.Sprintf("login init HTTP %d", resp.StatusCode))
		return
	}
	var wrap struct {
		Data struct {
			FlowID       string `json:"flow_id"`
			AuthorizeURL string `json:"authorize_url"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &wrap) != nil || wrap.Data.FlowID == "" || wrap.Data.AuthorizeURL == "" {
		writeJSONError(w, http.StatusBadGateway, "incomplete OAuth flow data")
		return
	}
	s.mu.Lock()
	if s.loginFlows == nil {
		s.loginFlows = map[string]*loginFlow{}
	}
	s.loginFlows[wrap.Data.FlowID] = flow
	s.mu.Unlock()
	writeJSON(w, map[string]any{
		"flow_id":       wrap.Data.FlowID,
		"authorize_url": wrap.Data.AuthorizeURL,
		"expires_in":    int(loginFlowTTL.Seconds()),
	})
}

// apiLoginPoll: GET /api/login/poll/{flow_id} → pending/ready/failed/expired
func (s *Server) apiLoginPoll(w http.ResponseWriter, r *http.Request) {
	s.loginGC()
	flowID := strings.TrimPrefix(r.URL.Path, "/api/login/poll/")
	s.mu.Lock()
	flow := s.loginFlows[flowID]
	s.mu.Unlock()
	if flow == nil {
		writeJSON(w, map[string]any{"status": "expired"})
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet,
		s.oauthBase()+"/oauth/cli/poll/"+flowID, nil)
	req.Header.Set("Authorization", "Bearer "+flow.pollToken)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		writeJSON(w, map[string]any{"status": "pending"}) // a blip is pending
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		writeJSON(w, map[string]any{"status": "pending"})
		return
	}
	var wrap struct {
		Data struct {
			Status   string `json:"status"`
			Message  string `json:"message"`
			Reason   string `json:"reason"`
			Token    string `json:"token"`
			UserInfo struct {
				Email string `json:"email"`
			} `json:"user"`
			Zai struct {
				AccessToken string `json:"access_token"`
			} `json:"zai"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &wrap) != nil {
		writeJSON(w, map[string]any{"status": "pending"})
		return
	}
	d := wrap.Data
	switch d.Status {
	case "failed":
		s.mu.Lock()
		delete(s.loginFlows, flowID)
		s.mu.Unlock()
		msg := d.Message
		if msg == "" {
			msg = d.Reason
		}
		if msg == "" {
			msg = "authorization failed or was denied"
		}
		writeJSON(w, map[string]any{"status": "failed", "message": msg})
		return
	case "ready":
		// detach before the long inline exchange so double-polls can't re-enter
		s.mu.Lock()
		delete(s.loginFlows, flowID)
		s.mu.Unlock()
	default:
		writeJSON(w, map[string]any{"status": "pending"})
		return
	}

	label := flow.label
	if label == "" {
		label = "oauth-login"
	}
	if d.UserInfo.Email != "" {
		label = d.UserInfo.Email
	}
	if d.Token == "" {
		writeJSON(w, map[string]any{"status": "failed", "message": "no credentials in the authorization result"})
		return
	}
	if s.store.HasJWT(d.Token) {
		writeJSON(w, map[string]any{"status": "ready", "account": label, "added": false})
		return
	}
	acc, err := s.store.Add(d.Token, label)
	if err != nil {
		writeJSON(w, map[string]any{"status": "failed", "message": err.Error()})
		return
	}
	// same-account API-key fallback (best effort; JWT already pooled)
	apiKeyNote := ""
	if d.Zai.AccessToken != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		key, kerr := s.exchangeAPIKey(ctx, d.Zai.AccessToken)
		cancel()
		if kerr == nil {
			s.store.SetAPIKey(acc.JWT, key)
			apiKeyNote = "api-key ok"
		} else {
			apiKeyNote = "api-key exchange failed: " + kerr.Error()
		}
	}
	// auto-warm right after authorization (events + offered-plan claims)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		rep := s.up.WarmAccount(ctx, acc.JWT, acc.DeviceMid)
		cancel()
		log.Printf("[login] %s warmed: %s %s", label, rep.Plans, rep.Events)
	}()
	log.Printf("[login] account added via OAuth: %s (%s)", label, apiKeyNote)
	writeJSON(w, map[string]any{"status": "ready", "account": label, "added": true, "api_key": apiKeyNote})
}

// exchangeAPIKey ports ZaiAuthFlow.exchange_api_key: access_token →
// business token → default org/project → zcode-api-key → secret.
func (s *Server) exchangeAPIKey(ctx context.Context, accessToken string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	origin := s.exchangeOrigin()

	bizToken, err := func() (string, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, origin+"/api/auth/z/login",
			bytes.NewReader([]byte(`{"token":"`+accessToken+`"}`)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode != 200 {
			return "", fmt.Errorf("z/login HTTP %d", resp.StatusCode)
		}
		var wrap struct {
			Data struct {
				AccessToken string `json:"access_token"`
				AccessTokne string `json:"accessToken"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &wrap) != nil {
			return "", fmt.Errorf("z/login: bad json")
		}
		if wrap.Data.AccessToken != "" {
			return wrap.Data.AccessToken, nil
		}
		return wrap.Data.AccessTokne, nil
	}()
	if err != nil {
		return "", err
	}
	if bizToken == "" {
		return "", fmt.Errorf("no business token")
	}

	getJSON := func(url string) ([]byte, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		req.Header.Set("Authorization", "Bearer "+bizToken)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("%s HTTP %d", url, resp.StatusCode)
		}
		return raw, nil
	}

	infoRaw, err := getJSON(origin + "/api/biz/customer/getCustomerInfo")
	if err != nil {
		return "", err
	}
	var info struct {
		Data struct {
			Organizations []struct {
				OrganizationID   string `json:"organizationId"`
				OrganizationName string `json:"organizationName"`
				Projects         []struct {
					ProjectID   string `json:"projectId"`
					ProjectName string `json:"projectName"`
				} `json:"projects"`
			} `json:"organizations"`
		} `json:"data"`
	}
	if json.Unmarshal(infoRaw, &info) != nil {
		return "", fmt.Errorf("getCustomerInfo: bad json")
	}
	orgs := info.Data.Organizations
	if len(orgs) == 0 {
		return "", fmt.Errorf("no usable organization")
	}
	org := orgs[0]
	for _, o := range orgs {
		if strings.Contains(o.OrganizationName, "默认机构") {
			org = o
			break
		}
	}
	if len(org.Projects) == 0 {
		return "", fmt.Errorf("no usable project")
	}
	proj := org.Projects[0]
	for _, p := range org.Projects {
		if strings.Contains(p.ProjectName, "默认项目") {
			proj = p
			break
		}
	}

	keyURL := origin + "/api/biz/v1/organization/" + org.OrganizationID + "/projects/" + proj.ProjectID + "/api_keys"
	keysRaw, err := getJSON(keyURL)
	if err != nil {
		return "", err
	}
	var keys struct {
		Data []struct {
			Name   string `json:"name"`
			APIKey string `json:"apiKey"`
		} `json:"data"`
	}
	if json.Unmarshal(keysRaw, &keys) != nil {
		return "", fmt.Errorf("api_keys: bad json")
	}
	var apiKey string
	for _, k := range keys.Data {
		if k.Name == "zcode-api-key" {
			apiKey = k.APIKey
			break
		}
	}
	if apiKey == "" {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, keyURL,
			bytes.NewReader([]byte(`{"name":"zcode-api-key"}`)))
		req.Header.Set("Authorization", "Bearer "+bizToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode != 200 {
			return "", fmt.Errorf("api_keys create HTTP %d", resp.StatusCode)
		}
		var wrap struct {
			Data struct {
				APIKey string `json:"apiKey"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &wrap) != nil || wrap.Data.APIKey == "" {
			return "", fmt.Errorf("api_keys create: no key")
		}
		apiKey = wrap.Data.APIKey
	}

	copyRaw, err := getJSON(keyURL + "/copy/" + apiKey)
	if err != nil {
		return "", err
	}
	var copyWrap struct {
		Data struct {
			SecretKey string `json:"secretKey"`
		} `json:"data"`
	}
	if json.Unmarshal(copyRaw, &copyWrap) != nil || copyWrap.Data.SecretKey == "" {
		return "", fmt.Errorf("copy: no secretKey")
	}
	return apiKey + "." + copyWrap.Data.SecretKey, nil
}
