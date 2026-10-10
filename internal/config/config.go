// Package config loads the panel configuration from environment variables
// (and a local .env file, same convention as the hw reference panel).
package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config is loaded from environment variables.
type Config struct {
	Port          string
	AdminUser     string
	AdminPassword string
	DataDir       string
	PublicURL     string

	// APIKey is the downstream credential: clients must send
	// Authorization: Bearer <APIKey> to use /v1/*. Empty = open access.
	APIKey string

	// UpstreamBase is the bailucode OpenAPI base URL.
	UpstreamBase string

	// QuotaSyncInterval controls the keepalive worker cadence.
	QuotaSyncInterval time.Duration
	// QuotaRefreshEnabled: keepalive 顺带登录控制台刷新额度快照（需账号密码）。
	QuotaRefreshEnabled bool
	// KeepalivePingPath is the lightweight authenticated endpoint used by the
	// keepalive worker.
	// TODO(bailu-console): 确认一个稳定、轻量的鉴权探测端点（当前暂用 /models，已探查有效）。
	KeepalivePingPath string

	// CheckinEnabled toggles the 08:00/20:00 Asia/Shanghai check-in scheduler.
	CheckinEnabled bool

	// PanelBase is the bailucode control panel origin (login + claim).
	PanelBase string

	// CheckinSolver picks the Turnstile solver for the claim flow:
	// browser | capsolver | manual.
	CheckinSolver string
	// CapsolverKey is the paid solver API key (CheckinSolver=capsolver).
	CapsolverKey string
	// ChromePath is used by the browser helper (CheckinSolver=browser).
	ChromePath string
	// ClaimHelperPath is the bundled scripts/claim_helper.js path.
	ClaimHelperPath string

	// MaxInflight caps concurrent upstream requests per account.
	MaxInflight int
	// MaxRequestBytes caps downstream /v1/chat/completions request bodies.
	MaxRequestBytes int
	// UpstreamTimeout bounds a single upstream request.
	UpstreamTimeout time.Duration
}

func Get() Config {
	loadDotEnv(".env")
	cfg := Config{
		Port:              envOr("PORT", "18790"),
		AdminUser:         envOr("ADMIN_USERNAME", "admin"),
		AdminPassword:     envOr("ADMIN_PASSWORD", "admin123"),
		DataDir:           envOr("DATA_DIR", "./data"),
		PublicURL:         strings.TrimRight(envOr("PUBLIC_URL", "http://localhost:18790"), "/"),
		APIKey:            os.Getenv("API_KEY"),
		UpstreamBase:      envOr("UPSTREAM_BASE", "https://bailucode.com/openapi/v1"),
		QuotaSyncInterval: envDuration("QUOTA_SYNC_INTERVAL", 30*time.Minute),
		QuotaRefreshEnabled: envBool("QUOTA_REFRESH_ENABLED", true),
		KeepalivePingPath: envOr("KEEPALIVE_PING_PATH", "/models"),
		CheckinEnabled:    envBool("CHECKIN_ENABLED", true),
		PanelBase:         envOr("PANEL_BASE", "https://bailucode.com"),
		CheckinSolver:     envOr("TURNSTILE_SOLVER", "browser"),
		CapsolverKey:      os.Getenv("CAPSOLVER_KEY"),
		ChromePath:        envOr("CHROME_PATH", "/usr/bin/google-chrome"),
		ClaimHelperPath:   envOr("CLAIM_HELPER_PATH", "./scripts/claim_helper.js"),
		MaxInflight:       envInt("MAX_INFLIGHT", 4),
		MaxRequestBytes:   envInt("MAX_REQUEST_BYTES", 8<<20),
		UpstreamTimeout:   envDuration("UPSTREAM_TIMEOUT", 120*time.Second),
	}
	if cfg.PublicURL == "" {
		cfg.PublicURL = "http://localhost:" + cfg.Port
	}
	return cfg
}

func envOr(key, def string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	return v
}

func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n := 0
	neg := false
	for i, c := range v {
		if i == 0 && c == '-' {
			neg = true
			continue
		}
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	if neg {
		n = -n
	}
	return n
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return def
	}
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

func envDuration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// loadDotEnv applies KEY=VALUE from a local .env file, overriding process env.
func loadDotEnv(name string) {
	f, err := os.Open(name)
	if err != nil {
		if wd, err2 := os.Getwd(); err2 == nil {
			f, err = os.Open(filepath.Join(wd, name))
		}
	}
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
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 {
			if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
				v = v[1 : len(v)-1]
			}
		}
		if k != "" {
			_ = os.Setenv(k, v)
		}
	}
}
