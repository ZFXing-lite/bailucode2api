package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"bailucode2api/internal/checkin"
	"bailucode2api/internal/store"
)

// handleListAccounts returns all accounts with secrets redacted.
func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	accounts := s.db.List()
	views := make([]map[string]any, 0, len(accounts))
	for _, a := range accounts {
		views = append(views, a.PublicView())
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": views, "total": len(views)})
}

// handleImportAccounts accepts credential lines:
//
//	email----password----apikey
//	email----password            (apikey empty → pending_key)
//
// Dedup is always on: a line whose email already exists (in the pool or
// earlier in this batch) is reported as a duplicate and skipped.
func (s *Server) handleImportAccounts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "application/json") || strings.Contains(ct, "+json") {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "bad json")
			return
		}
	} else {
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "read body failed")
			return
		}
		req.Text = string(b)
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		writeErr(w, http.StatusBadRequest, "text required (email----password----apikey)")
		return
	}

	type rowResult struct {
		Email  string `json:"email"`
		Action string `json:"action"` // created|updated|duplicate|error
		ID     string `json:"id,omitempty"`
		Error  string `json:"error,omitempty"`
		Line   string `json:"line,omitempty"`
	}
	var (
		results             []rowResult
		created, updated    int
		duplicates, invalid int
	)
	existing := map[string]bool{}
	for _, a := range s.db.List() {
		if e := strings.ToLower(strings.TrimSpace(a.Email)); e != "" {
			existing[e] = true
		}
	}
	seen := map[string]bool{}

	for _, raw := range strings.Split(text, "\n") {
		raw = strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		line, ok := store.ParseCredentialLine(raw)
		if !ok {
			invalid++
			results = append(results, rowResult{Action: "error", Error: "unrecognized line", Line: raw})
			continue
		}
		key := strings.ToLower(line.Email)
		if existing[key] || seen[key] {
			duplicates++
			results = append(results, rowResult{Email: line.Email, Action: "duplicate"})
			continue
		}
		seen[key] = true

		status := store.StatusActive
		if strings.TrimSpace(line.ApiKey) == "" {
			status = store.StatusPendingKey
		}
		a := &store.Account{
			ID:       store.NewID(),
			Email:    line.Email,
			Password: line.Password,
			ApiKey:   line.ApiKey,
			Status:   status,
		}
		if err := s.db.Upsert(a); err != nil {
			results = append(results, rowResult{Email: line.Email, Action: "error", Error: err.Error()})
			continue
		}
		created++
		results = append(results, rowResult{Email: line.Email, Action: "created", ID: a.ID})
		_ = updated
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"created": created, "updated": updated,
		"duplicates": duplicates, "invalid": invalid,
		"results": results,
	})
}

func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, err := s.db.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	view := a.PublicView()
	if err := s.db.Delete(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": view})
}

// handleManualCheckin triggers the idempotent check-in for one account.
func (s *Server) handleManualCheckin(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, err := s.db.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	// browser 求解需要真实 Chrome 跑 Turnstile，给足时间。
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()
	alreadyDone, results, _ := checkin.Checkin(ctx, s.settings, a)
	// Persist real outcomes to LastCheckin + quota snapshot.
	now := time.Now()
	if a.LastCheckin == nil {
		a.LastCheckin = map[string]time.Time{}
	}
	changed := false
	for _, res := range results {
		if res.Claimed || res.AlreadyDone {
			a.LastCheckin[res.Slot] = now
			changed = true
		}
	}
	if a.Quota != nil {
		changed = true
	}
	if changed {
		_ = s.db.Upsert(a)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"already_done": alreadyDone,
		"solver":       s.settings.SolverKind,
		"results":      results,
		"quota":         a.Quota,
	})
}

// handleRunCheckin triggers an immediate full check-in pass for all accounts.
func (s *Server) handleRunCheckin(w http.ResponseWriter, r *http.Request) {
	go s.checkin.RunOnce(context.Background())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "checkin pass started"})
}

// handleRefreshAccountQuota refreshes one account's control-panel quota
// snapshot (pure HTTP login + get_stats; no claim, no captcha).
func (s *Server) handleRefreshAccountQuota(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, err := s.db.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	snap, err := s.refreshQuota(ctx, a)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "quota": snap})
}

// handleRefreshAllQuota refreshes every account's quota snapshot in the
// background and returns immediately.
func (s *Server) handleRefreshAllQuota(w http.ResponseWriter, r *http.Request) {
	go s.refreshAllQuotas()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "quota refresh started"})
}
