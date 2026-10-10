package api

import (
	"encoding/json"
	"net/http"
	"time"

	"bailucode2api/internal/store"
)

// handleListKeys returns all downstream keys with per-key usage rollups.
func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	keys := s.db.ListApiKeys()
	stats := s.db.UsageStats(0)
	byKey, _ := stats["by_key"].(map[string]*store.UsageCell)
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		cell := byKey[k.ID]
		if cell == nil {
			cell = &store.UsageCell{}
		}
		out = append(out, map[string]any{
			"id":          k.ID,
			"name":        k.Name,
			"prefix":      k.Prefix,
			"enabled":     k.Enabled,
			"created_at":  k.CreatedAt,
			"requests":    cell.Requests,
			"tokens_in":   cell.TokensIn,
			"tokens_out":  cell.TokensOut,
			"tokens_total": cell.TokensTotal,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

// handleCreateKey creates a downstream key. Body: {name, secret?}.
// Empty secret auto-generates sk-b2a-…; the full secret is returned exactly
// once.
func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string `json:"name"`
		Secret string `json:"secret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	key, err := s.db.CreateApiKey(req.Name, req.Secret)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":     key.ID,
		"name":   key.Name,
		"secret": key.Secret, // 完整 key 仅本次返回
		"prefix": key.Prefix,
	})
}

// handleDeleteKey removes a downstream key.
func (s *Server) handleDeleteKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.db.DeleteApiKey(id); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleToggleKey enables/disables a downstream key.
func (s *Server) handleToggleKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	key, err := s.db.ToggleApiKey(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": key.Enabled})
}

// handleUsageStats returns token usage: totals, today, last-14-days series,
// per-key and per-account breakdowns.
func (s *Server) handleUsageStats(w http.ResponseWriter, r *http.Request) {
	stats := s.db.UsageStats(14)
	// by_key 附上 key 名称与账号邮箱，前端直接渲染。
	byKey, _ := stats["by_key"].(map[string]*store.UsageCell)
	rows := make([]map[string]any, 0, len(byKey))
	for _, k := range s.db.ListApiKeys() {
		cell := byKey[k.ID]
		if cell == nil {
			cell = &store.UsageCell{}
		}
		rows = append(rows, map[string]any{
			"id": k.ID, "name": k.Name, "enabled": k.Enabled,
			"requests": cell.Requests, "tokens_in": cell.TokensIn,
			"tokens_out": cell.TokensOut, "tokens_total": cell.TokensTotal,
		})
	}
	stats["by_key_rows"] = rows

	byAccount, _ := stats["by_account"].(map[string]*store.UsageCell)
	accRows := make([]map[string]any, 0, len(byAccount))
	for _, a := range s.db.List() {
		cell := byAccount[a.ID]
		if cell == nil {
			continue
		}
		accRows = append(accRows, map[string]any{
			"id": a.ID, "email": a.Email,
			"requests": cell.Requests, "tokens_in": cell.TokensIn,
			"tokens_out": cell.TokensOut, "tokens_total": cell.TokensTotal,
		})
	}
	stats["by_account_rows"] = accRows
	stats["generated_at"] = time.Now().Format(time.RFC3339)
	writeJSON(w, http.StatusOK, stats)
}
