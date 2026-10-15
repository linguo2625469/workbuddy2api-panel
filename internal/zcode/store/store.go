package store

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"github.com/linguo2625469/workbuddy2api-panel/internal/zcode/util"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

type Account struct {
	APIKey         string
	RiskStrikes    int64
	CoolingUntil   int64
	DisabledModels string // comma-separated model norms excluded from dispatch
	ID             int64
	JWT            string
	Label          string
	DeviceMid      string
	QuotaJSON      string
	PlanName       string
	ExpiresAt      string
	Active         bool
	Status         string // "", "expired", "revoked" — marked accounts are hidden, never auto-deleted
}

type Store struct {
	rotMu  sync.Mutex
	rotIdx map[string]int // per-model rotation cursor (norm(model) → last index)
	mu     sync.Mutex
	db     *sql.DB
}

func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && filepath.Dir(path) != "." {
		// ok if dir is .
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{
		rotIdx: map[string]int{}, db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS accounts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  jwt TEXT NOT NULL UNIQUE,
  label TEXT DEFAULT '',
  device_mid TEXT DEFAULT '',
  quota_json TEXT DEFAULT '',
  plan_name TEXT DEFAULT '',
  expires_at TEXT DEFAULT '',
  active INTEGER DEFAULT 0,
  status TEXT NOT NULL DEFAULT '',
  api_key TEXT DEFAULT '',
  risk_strikes INTEGER DEFAULT 0,
  cooling_until INTEGER DEFAULT 0,
  disabled_models TEXT DEFAULT ''
);
CREATE TABLE IF NOT EXISTS dead (
  account_id INTEGER NOT NULL,
  model_norm TEXT NOT NULL,
  day TEXT NOT NULL,
  PRIMARY KEY (account_id, model_norm, day)
);
CREATE TABLE IF NOT EXISTS dead_until (
  account_id INTEGER NOT NULL,
  model_norm TEXT NOT NULL,
  until INTEGER NOT NULL,
  PRIMARY KEY (account_id, model_norm)
);
CREATE TABLE IF NOT EXISTS settings (
  k TEXT PRIMARY KEY,
  v TEXT NOT NULL
);`)
	if err != nil {
		return err
	}
	// existing databases: add the status column (ignored when already present)
	if _, err = s.db.Exec(`ALTER TABLE accounts ADD COLUMN status TEXT NOT NULL DEFAULT ''`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	// 2026-09-13: OAuth api_key + risk-strike/cooling (same ignore-if-present)
	for _, col := range []string{
		`ALTER TABLE accounts ADD COLUMN api_key TEXT DEFAULT ''`,
		`ALTER TABLE accounts ADD COLUMN risk_strikes INTEGER DEFAULT 0`,
		`ALTER TABLE accounts ADD COLUMN cooling_until INTEGER DEFAULT 0`,
		`ALTER TABLE accounts ADD COLUMN disabled_models TEXT DEFAULT ''`,
	} {
		if _, err = s.db.Exec(col); err != nil {
			if !strings.Contains(err.Error(), "duplicate column") {
				return err
			}
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

const accountCols = `id, jwt, label, device_mid, quota_json, plan_name, expires_at, active, status, api_key, risk_strikes, cooling_until, disabled_models`

func scanAccount(sc interface{ Scan(...any) error }) (Account, error) {
	var a Account
	var active int
	err := sc.Scan(&a.ID, &a.JWT, &a.Label, &a.DeviceMid, &a.QuotaJSON, &a.PlanName, &a.ExpiresAt, &active, &a.Status, &a.APIKey, &a.RiskStrikes, &a.CoolingUntil, &a.DisabledModels)
	a.Active = active != 0
	return a, err
}

func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&n)
	return n
}

func (s *Store) Active() *Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := scanAccount(s.db.QueryRow(`SELECT ` + accountCols + ` FROM accounts WHERE active=1 ORDER BY id LIMIT 1`))
	if err == nil {
		return &a
	}
	a, err = scanAccount(s.db.QueryRow(`SELECT ` + accountCols + ` FROM accounts ORDER BY id LIMIT 1`))
	if err != nil {
		return nil
	}
	return &a
}

func (s *Store) Accounts() ([]Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT ` + accountCols + ` FROM accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) Add(jwt, label string) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// fingerprint pattern: a fresh uuid4 per account — one "fresh
	// install on this machine" per account, not a derived/synthetic scheme
	mid := util.GenerateUUID()
	res, err := s.db.Exec(`INSERT INTO accounts (jwt, label, device_mid) VALUES (?, ?, ?)`, jwt, label, mid)
	if err != nil {
		return Account{}, err
	}
	id, _ := res.LastInsertId()
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&n)
	if n == 1 {
		_, _ = s.db.Exec(`UPDATE accounts SET active=1 WHERE id=?`, id)
	}
	return Account{ID: id, JWT: jwt, Label: label, DeviceMid: mid, Active: n == 1}, nil
}

func (s *Store) Remove(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var was int
	_ = s.db.QueryRow(`SELECT active FROM accounts WHERE id=?`, id).Scan(&was)
	_, err := s.db.Exec(`DELETE FROM accounts WHERE id=?`, id)
	if err != nil {
		return err
	}
	if was != 0 {
		_, _ = s.db.Exec(`UPDATE accounts SET active=1 WHERE id=(SELECT MIN(id) FROM accounts)`)
	}
	return nil
}

// MarkEmptyFreeExpired: stored Free with no entitlements are marked expired
// and excluded from rotation — never auto-deleted. Unchecked imports
// (plan_name=”) stay untouched.
func (s *Store) MarkEmptyFreeExpired() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`
UPDATE accounts SET status='expired', active=0
 WHERE status=''
   AND lower(trim(plan_name)) = 'free'
   AND (quota_json IS NULL OR trim(quota_json) IN ('', '[]', 'null'))`)
	if err != nil {
		return 0
	}
	n, _ := res.RowsAffected()
	return n
}

// MarkStatus flags accounts (expired/revoked) and deactivates them; the rows
// stay in the database — only the dashboard remove button deletes.
func (s *Store) MarkStatus(ids []int64, status string) int {
	if len(ids) == 0 {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	args := make([]any, 0, len(ids)+1)
	args = append(args, status)
	ph := make([]byte, 0, len(ids)*2)
	for i, id := range ids {
		if i > 0 {
			ph = append(ph, ',')
		}
		ph = append(ph, '?')
		args = append(args, id)
	}
	res, err := s.db.Exec(`UPDATE accounts SET status=?, active=0 WHERE id IN (`+string(ph)+`)`, args...)
	if err != nil {
		return 0
	}
	n, _ := res.RowsAffected()
	return int(n)
}

func (s *Store) Select(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE accounts SET active=0`); err != nil {
		tx.Rollback()
		return err
	}
	res, err := tx.Exec(`UPDATE accounts SET active=1 WHERE id=?`, id)
	if err != nil {
		tx.Rollback()
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		tx.Rollback()
		return fmt.Errorf("account %d not found", id)
	}
	return tx.Commit()
}

func (s *Store) UpdateQuota(id int64, q Quota) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.Marshal(q.Entitlements)
	// a real billing snapshot clears stale expired/revoked marks (the account
	// came back — e.g. after warming delivered the grant); the operator's
	// disabled mark survives refreshes
	_, err := s.db.Exec(`UPDATE accounts SET quota_json=?, plan_name=?, expires_at=?,
status=CASE WHEN status='disabled' THEN 'disabled' ELSE '' END WHERE id=?`,
		string(b), q.PlanName, q.ExpiresAt, id)
	return err
}

// Quota — an account's billing snapshot (fetched by upstream, persisted here).
type Quota struct {
	Remaining    int              `json:"remaining"`
	Used         int              `json:"used"`
	Limit        int              `json:"limit"`
	PlanName     string           `json:"membershipType"`
	ExpiresAt    string           `json:"expiresAt"`
	Balances     []map[string]any `json:"balances"`
	Entitlements []map[string]any `json:"entitlements"`
}

func (s *Store) MarkDead(accountID int64, modelNorm string, until int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if until <= 0 {
		until = time.Now().Unix() + 86400
	}
	_, _ = s.db.Exec(`INSERT INTO dead_until(account_id, model_norm, until) VALUES (?,?,?)
ON CONFLICT(account_id, model_norm) DO UPDATE SET until=excluded.until`, accountID, modelNorm, until)
}

func (s *Store) DeadSet() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	rows, err := s.db.Query(`SELECT account_id, model_norm FROM dead_until WHERE until > ?`, now)
	if err != nil {
		return map[string]bool{}
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id int64
		var m string
		if rows.Scan(&id, &m) == nil {
			out[fmt.Sprintf("%d:%s", id, m)] = true
		}
	}
	return out
}

func (s *Store) DeadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM dead_until WHERE until > ?`, time.Now().Unix()).Scan(&n)
	return n
}

func (s *Store) Setting(k, def string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var v string
	err := s.db.QueryRow(`SELECT v FROM settings WHERE k=?`, k).Scan(&v)
	if err != nil {
		return def
	}
	return v
}

func (s *Store) SetSetting(k, v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.db.Exec(`INSERT INTO settings(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
}

// DeviceMidFor ports the manager gateway's X-Device-Mid scheme: uuid5 in
// NAMESPACE_DNS over "zcode-gw." + account id, stable per account. The
// gateway keys it by its DB row id; the stable account identity here is the
// JWT sub, with the whole jwt as fallback when the claim is missing.
func DeviceMidFor(jwt string) string {
	name := jwt
	if sub := jwtUser(jwt); sub != "" {
		name = sub
	}
	return uuid.NewSHA1(uuid.NameSpaceDNS, []byte("zcode-gw."+name)).String()
}

// JWTSub extracts the subject (sub, then user_id) from a JWT without
// verifying it — metadata.user_id carries this plain string
// body_transform.jwt_user_id).
func JWTSub(jwt string) string {
	return jwtUser(jwt)
}

func jwtUser(jwt string) string {
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	return claims.Sub
}
