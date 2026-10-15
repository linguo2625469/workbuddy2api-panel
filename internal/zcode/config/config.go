package config

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"time"
)

// AppVersion — single source for the startup banner and the dashboard.
// Override at build: go build -ldflags "-X github.com/linguo2625469/workbuddy2api-panel/internal/zcode/config.AppVersion=1.0.1"
var AppVersion = "1.0.1"

// Deployment constants (hardwired; not env-driven). OriginBaseURL and
// ClientVersion are vars only so tests can point them at a mock upstream.
var (
	AccountsDBPath = "data/zcode-accounts.db"
	KeysFilePath   = env("ZCODE_KEYS", "tokens.txt")
	OriginBaseURL  = "https://zcode.z.ai"
	ClientVersion  = "3.14.0" // messages fingerprint (verified 200)
	// BillingClientVersion — the billing family (balance/preview/claim +
	// activation events) identifies as the current desktop, 3.11.2; the
	// platform gates plan offers by client version (empirically proven split).
	BillingClientVersion = "3.14.0"
	DefaultModelID       = "GLM-5.3"
)

type Config struct {
	Host           string
	Port           string
	AuthToken      string
	ProxyFile      string
	LoadSwaps      int // LOAD_SWAPS: egress rotations on a load verdict (attempts = swaps+1)
	AccountMaxTry  int // ACCOUNT_MAX_TRY: max accounts tried per client request
	BillingWorkers int
	GatewayKey     string // GATEWAY_KEY: separate Bearer for /v1/* (empty = AUTH_TOKEN)
	CaptchaOff     bool   // CAPTCHA_MODE=off
	CaptchaTimeout time.Duration
	CaptchaRetries int
}

func LoadConfig() Config {
	c := Config{
		Host:           env("HOST", "127.0.0.1"),
		Port:           env("PORT", "3101"),
		AuthToken:      env("AUTH_TOKEN", "d3vin"),
		ProxyFile:      env("PROXY_FILE", "proxy.txt"),
		LoadSwaps:      4,
		AccountMaxTry:  8,
		BillingWorkers: 8,
		GatewayKey:     env("GATEWAY_KEY", ""),
		CaptchaOff:     os.Getenv("CAPTCHA_MODE") == "off",
		CaptchaTimeout: 40 * time.Second,
		CaptchaRetries: 3,
	}
	if v := os.Getenv("BILLING_WORKERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.BillingWorkers = n
		}
	}
	// LOAD_SWAPS=0 is legal: single attempt, no egress rotation
	if v := os.Getenv("LOAD_SWAPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.LoadSwaps = n
		}
	}
	if v := os.Getenv("ACCOUNT_MAX_TRY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.AccountMaxTry = n
		}
	}
	if v := env("CAPTCHA_SOLVE_TIMEOUT", ""); v != "" {
		if d := envDuration(v); d > 0 {
			c.CaptchaTimeout = d
		}
	}
	if v := os.Getenv("CAPTCHA_RETRIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.CaptchaRetries = n
		}
	}
	return c
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

// LoadDotEnv reads .env into the process env (first writer wins: a real
// env var beats the file). A missing file is fine.
func LoadDotEnv() {
	f, err := os.Open(".env")
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}

func envDuration(v string) time.Duration {
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if n, err := strconv.Atoi(strings.TrimSuffix(v, "s")); err == nil {
		return time.Duration(n) * time.Second
	}
	return 0
}

// ScrubMitmEnv removes this machine's local-mitmproxy env (http(s)_proxy,
// SSL_CERT_FILE) so Go TLS goes direct with system roots. Idempotent.
func ScrubMitmEnv() {
	for _, k := range []string{
		"http_proxy", "https_proxy", "all_proxy",
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
		"SSL_CERT_FILE", "SSL_CERT_DIR",
	} {
		os.Unsetenv(k)
	}
}

// LoadKeyFile reads a JWT-per-line keys file.
func LoadKeyFile(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}
