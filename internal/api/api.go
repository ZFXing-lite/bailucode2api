// Package api is the HTTP layer: admin UI/API plus the OpenAI-compatible
// gateway (/v1/*). Structure follows the hw reference panel, trimmed down.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"bailucode2api/internal/auth"
	"bailucode2api/internal/bailu"
	"bailucode2api/internal/checkin"
	"bailucode2api/internal/config"
	"bailucode2api/internal/pool"
	"bailucode2api/internal/store"
	"bailucode2api/internal/turnstile"
	"bailucode2api/internal/upstream"
	"bailucode2api/internal/webui"
)

// Server wires config, storage, account pool and upstream together.
type Server struct {
	cfg      config.Config
	db       *store.DB
	auth     *auth.Manager
	pool     *pool.Pool
	upstream *upstream.Client
	checkin  *checkin.Scheduler
	settings checkin.Settings
}

func New(cfg config.Config, db *store.DB, authMgr *auth.Manager) *Server {
	settings := checkin.Settings{
		PanelBase:  cfg.PanelBase,
		SolverKind: cfg.CheckinSolver,
	}
	switch settings.SolverKind {
	case "capsolver":
		if cfg.CapsolverKey != "" {
			settings.Capsolver = turnstile.NewCapsolver(cfg.CapsolverKey)
		} else {
			log.Printf("checkin: TURNSTILE_SOLVER=capsolver but CAPSOLVER_KEY empty → manual")
			settings.SolverKind = "manual"
		}
	case "browser":
		helper := cfg.ClaimHelperPath
		if helper == "" {
			helper = "scripts/claim_helper.js"
		}
		settings.Browser = turnstile.NewBrowser(helper, cfg.ChromePath, "")
	default:
		settings.SolverKind = "manual"
	}
	return &Server{
		cfg:      cfg,
		db:       db,
		auth:     authMgr,
		pool:     pool.New(db, cfg.MaxInflight),
		upstream: upstream.New(cfg.UpstreamBase, cfg.UpstreamTimeout),
		checkin:  checkin.NewScheduler(db, cfg.CheckinEnabled, settings),
		settings: settings,
	}
}

// StartCheckinScheduler launches the 08:00/20:00 Asia/Shanghai check-in loop.
func (s *Server) StartCheckinScheduler(ctx context.Context) {
	s.checkin.Start(ctx)
}

// StartKeepalive launches the 30-minute keepalive worker: a lightweight
// authenticated probe per account keeps sessions warm and marks dead keys.
// (Mirrors the hw panel's quota-sync cadence and workbuddy2api's token保活.)
func (s *Server) StartKeepalive(ctx context.Context) {
	interval := s.cfg.QuotaSyncInterval
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	log.Printf("keepalive: every %s, ping=%s", interval, s.cfg.KeepalivePingPath)
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(15 * time.Second):
			s.keepaliveOnce()
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.keepaliveOnce()
			}
		}
	}()
}

func (s *Server) keepaliveOnce() {
	// 两段式保活：
	//   1) 轻量 GET {base}/models 验证 API key（401/403 → 标死号）
	//   2) QUOTA_REFRESH_ENABLED=1 且账号有密码时，登录控制台 get_stats
	//      刷新额度快照（纯 HTTP，无需验证码）。
	ok, fail := 0, 0
	for _, a := range s.db.List() {
		if a == nil || !a.HasApiKey() || a.Disabled {
			continue
		}
		cctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		status, _, err := s.upstream.Models(cctx, a.ApiKey)
		cancel()
		now := time.Now()
		if err != nil {
			fail++
			a.FailCount++
			a.ConsecutiveFails++
			a.LastError = err.Error()
			a.UpdatedAt = now
			_ = s.db.Upsert(a)
			continue
		}
		if upstream.IsAuthError(status) {
			fail++
			// Key rejected: hard-mark so the pool stops using it.
			s.pool.MarkAuthError(a.ID, "keepalive: upstream rejected api key")
			continue
		}
		if status >= 500 {
			fail++
			a.LastError = "keepalive: upstream 5xx"
			_ = s.db.Upsert(a)
			continue
		}
		ok++
		if a.ConsecutiveFails > 0 || a.LastError != "" {
			a.ConsecutiveFails = 0
			a.LastError = ""
			a.UpdatedAt = now
			_ = s.db.Upsert(a)
		}
	}
	log.Printf("keepalive: done ok=%d fail=%d", ok, fail)
	if s.cfg.QuotaRefreshEnabled {
		s.refreshAllQuotas()
	}
}

// refreshAllQuotas walks every account that has a password and refreshes its
// control-panel quota snapshot (pure HTTP login + get_stats, no captcha).
func (s *Server) refreshAllQuotas() {
	accounts := s.db.List()
	refreshed, failed := 0, 0
	for _, a := range accounts {
		if a == nil || a.Disabled || a.Password == "" {
			continue
		}
		qctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		_, err := s.refreshQuota(qctx, a)
		cancel()
		if err != nil {
			failed++
			log.Printf("quota: refresh %s failed: %v", a.Email, err)
		} else {
			refreshed++
		}
	}
	log.Printf("quota: refresh done ok=%d fail=%d (interval=%.0fm)", refreshed, failed,
		s.cfg.QuotaSyncInterval.Minutes())
}

// refreshQuota logs into the control panel for one account, fetches get_stats
// and persists the snapshot. Returns the fresh snapshot.
func (s *Server) refreshQuota(ctx context.Context, a *store.Account) (*store.QuotaSnapshot, error) {
	if a == nil {
		return nil, errors.New("account nil")
	}
	if a.Password == "" {
		return nil, errors.New("账号未配置密码，无法登录控制台查询额度")
	}
	client := bailu.New(s.cfg.PanelBase, 60*time.Second)
	if err := client.Login(ctx, a.Email, a.Password); err != nil {
		return nil, fmt.Errorf("登录控制台: %w", err)
	}
	st, err := client.Stats(ctx)
	if err != nil {
		return nil, fmt.Errorf("额度查询: %w", err)
	}
	snap := bailu.SnapshotFromStats(st)
	a.Quota = snap
	a.UpdatedAt = time.Now()
	if err := s.db.Upsert(a); err != nil {
		return snap, err
	}
	return snap, nil
}

// Handler builds the route tree.
// /v1/* (gateway) uses downstream API-key auth; everything else goes through
// the admin session middleware.
func (s *Server) Handler() http.Handler {
	admin := http.NewServeMux()
	admin.HandleFunc("GET /health", s.handleHealth)
	admin.HandleFunc("GET /ready", s.handleHealth)
	admin.HandleFunc("GET /", s.serveIndex)
	admin.HandleFunc("GET /login", s.serveIndex)
	admin.Handle("GET /static/", webui.StaticHandler())

	admin.HandleFunc("POST /api/login", s.handleLogin)
	admin.HandleFunc("POST /api/logout", s.handleLogout)
	admin.HandleFunc("GET /api/dashboard", s.handleDashboard)

	admin.HandleFunc("GET /api/accounts", s.handleListAccounts)
	admin.HandleFunc("POST /api/accounts/import", s.handleImportAccounts)
	admin.HandleFunc("DELETE /api/accounts/{id}", s.handleDeleteAccount)
	admin.HandleFunc("POST /api/accounts/{id}/checkin", s.handleManualCheckin)
	admin.HandleFunc("POST /api/accounts/{id}/quota", s.handleRefreshAccountQuota)
	admin.HandleFunc("POST /api/checkin/run", s.handleRunCheckin)
	admin.HandleFunc("POST /api/quota/refresh", s.handleRefreshAllQuota)
	admin.HandleFunc("GET /api/keys", s.handleListKeys)
	admin.HandleFunc("POST /api/keys", s.handleCreateKey)
	admin.HandleFunc("DELETE /api/keys/{id}", s.handleDeleteKey)
	admin.HandleFunc("POST /api/keys/{id}/toggle", s.handleToggleKey)
	admin.HandleFunc("GET /api/stats", s.handleUsageStats)

	mux := http.NewServeMux()
	mux.Handle("/v1/", http.HandlerFunc(s.handleV1)) // gateway: own auth
	mux.Handle("/", s.auth.Middleware(admin))
	return mux
}

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(webui.IndexHTML())
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"service": "bailucode2api",
		"pool":    s.pool.Stats(),
		"solver":  s.settings.SolverKind,
		"time":    time.Now().Format(time.RFC3339),
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if strings.TrimSpace(req.Username) != s.cfg.AdminUser {
		writeErr(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	token, err := s.auth.Login(req.Password)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "admin_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "admin_session", Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	accounts := s.db.List()
	views := make([]map[string]any, 0, len(accounts))
	byStatus := map[string]int{}
	var quotaTotal, quotaUsed, quotaRemain float64
	var creditsTotal, creditsUsed float64
	for _, a := range accounts {
		views = append(views, a.PublicView())
		byStatus[a.Status]++
		if a.Quota != nil {
			quotaTotal += a.Quota.TotalTokens
			quotaUsed += a.Quota.UsedTokens
			quotaRemain += a.Quota.RemainTokens
			creditsTotal += a.Quota.CreditsQuota
			creditsUsed += a.Quota.CreditsUsed
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pool":      s.pool.Stats(),
		"by_status": byStatus,
		"accounts":  views,
		"upstream":  s.cfg.UpstreamBase,
		"solver":    s.settings.SolverKind,
		"quota": map[string]any{
			"total_tokens":  quotaTotal,
			"used_tokens":   quotaUsed,
			"remain_tokens": quotaRemain,
			"credits_total": creditsTotal,
			"credits_used":  creditsUsed,
		},
	})
}

// authorizeDownstream guards /v1/*: panel-managed keys first, then the legacy
// API_KEY env var, then open mode (no keys configured at all).
// Returns the matched panel key (nil for env/open) and whether to allow.
func (s *Server) authorizeDownstream(r *http.Request) (*store.ApiKey, bool) {
	got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer"))
	if got == "" {
		got = strings.TrimSpace(r.Header.Get("X-Api-Key"))
	}
	if got == "" {
		return nil, s.downstreamOpen()
	}
	if k := s.db.MatchApiKey(got); k != nil {
		return k, true
	}
	// legacy single-key fallback
	if strings.TrimSpace(s.cfg.APIKey) != "" &&
		subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.APIKey)) == 1 {
		return nil, true
	}
	return nil, false
}

// downstreamOpen reports whether /v1/* accepts any caller (no keys configured).
func (s *Server) downstreamOpen() bool {
	for _, k := range s.db.ListApiKeys() {
		if k != nil && k.Enabled {
			return false
		}
	}
	return strings.TrimSpace(s.cfg.APIKey) == ""
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}
