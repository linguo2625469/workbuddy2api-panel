package store

import (
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/zcode/util"
)

// HasJWT reports whether the account pool already contains this JWT.
func (s *Store) HasJWT(jwt string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM accounts WHERE jwt=?`, jwt).Scan(&n)
	return n > 0
}

// SetDeviceMid pins an account's device identity (the manager-gateway mid
// on import); the header must carry the device the edge already knows.
func (s *Store) SetDeviceMid(jwt, mid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.db.Exec(`UPDATE accounts SET device_mid=? WHERE jwt=?`, mid, jwt)
}

// SetAPIKey stores the OAuth-exchanged api.z.ai key as the account's
// same-account fallback credential.
func (s *Store) SetAPIKey(jwt, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.db.Exec(`UPDATE accounts SET api_key=? WHERE jwt=?`, key, jwt)
}

// ExportAccounts returns the full pool (jwt, label, device_mid, quota) for
// backup; ImportAccounts restores rows, keeping device identities.
func (s *Store) ExportAccounts() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, _ := s.db.Query(`SELECT jwt, label, device_mid, quota_json, plan_name, expires_at, api_key FROM accounts`)
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var jwt, label, mid, quota, plan, expires, apiKey string
		_ = rows.Scan(&jwt, &label, &mid, &quota, &plan, &expires, &apiKey)
		out = append(out, map[string]any{
			"jwt": jwt, "label": label, "device_mid": mid,
			"quota_json": quota, "plan_name": plan, "expires_at": expires, "api_key": apiKey,
		})
	}
	return out
}

// ImportAccounts adds rows verbatim (device_mid preserved) if the JWT is new.
func (s *Store) ImportAccounts(rows []map[string]any) (added, skipped int) {
	for _, r := range rows {
		jwt, _ := r["jwt"].(string)
		if jwt == "" {
			continue
		}
		if s.HasJWT(jwt) {
			skipped++
			continue
		}
		label, _ := r["label"].(string)
		mid, _ := r["device_mid"].(string)
		if mid == "" {
			mid = util.GenerateUUID()
		}
		quota, _ := r["quota_json"].(string)
		plan, _ := r["plan_name"].(string)
		expires, _ := r["expires_at"].(string)
		apiKey, _ := r["api_key"].(string)
		if _, err := s.db.Exec(`INSERT INTO accounts (jwt, label, device_mid, quota_json, plan_name, expires_at, api_key) VALUES (?,?,?,?,?,?,?)`,
			jwt, label, mid, quota, plan, expires, apiKey); err != nil {
			skipped++
			continue
		}
		added++
	}
	return added, skipped
}

// RiskStrike increments the account's 3012 counter; at threshold it cools
// the account for the given window.
// Returns the new strike count.
func (s *Store) RiskStrike(id int64, threshold int, cool time.Duration) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	_ = s.db.QueryRow(`SELECT risk_strikes FROM accounts WHERE id=?`, id).Scan(&n)
	n++
	if threshold > 0 && n >= int64(threshold) {
		_, _ = s.db.Exec(`UPDATE accounts SET risk_strikes=?, cooling_until=? WHERE id=?`,
			n, time.Now().Add(cool).Unix(), id)
	} else {
		_, _ = s.db.Exec(`UPDATE accounts SET risk_strikes=? WHERE id=?`, n, id)
	}
	return n
}

// RiskReset clears strikes (a clean 200 answers).
func (s *Store) RiskReset(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.db.Exec(`UPDATE accounts SET risk_strikes=0, cooling_until=0 WHERE id=?`, id)
}

// Rotate advances the named rotation cursor over n items and returns the
// new position (0..n-1). With n<=1 it stays 0.
func (s *Store) Rotate(key string, n int) int {
	s.rotMu.Lock()
	defer s.rotMu.Unlock()
	if n <= 1 {
		return 0
	}
	s.rotIdx[key] = (s.rotIdx[key] + 1) % n
	return s.rotIdx[key]
}

// SetLabel updates the account display name.
func (s *Store) SetLabel(id int64, label string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.db.Exec(`UPDATE accounts SET label=? WHERE id=?`, label, id)
}

// SetToken replaces the account JWT and resets its status.
func (s *Store) SetToken(id int64, jwt string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// re-saving a JWT is the operator's intentional unfreeze: status AND the
	// risk-control cooldown clear (cooling-freeze semantics of the reference)
	_, _ = s.db.Exec(`UPDATE accounts SET jwt=?, status='', risk_strikes=0, cooling_until=0 WHERE id=?`, jwt, id)
}

// SetDisabledModels pins the per-account model exclusion list (norms, csv).
func (s *Store) SetDisabledModels(id int64, csv string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.db.Exec(`UPDATE accounts SET disabled_models=? WHERE id=?`, csv, id)
}

// SetStatusByID sets/clears the hidden status ("disabled", "" = visible).
func (s *Store) SetStatusByID(id int64, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.db.Exec(`UPDATE accounts SET status=? WHERE id=?`, status, id)
}
