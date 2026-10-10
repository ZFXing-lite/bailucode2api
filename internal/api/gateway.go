package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"bailucode2api/internal/pool"
	"bailucode2api/internal/store"
	"bailucode2api/internal/upstream"
)

// handleV1 routes the OpenAI-compatible gateway surface.
func (s *Server) handleV1(w http.ResponseWriter, r *http.Request) {
	key, ok := s.authorizeDownstream(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	switch {
	case r.URL.Path == "/v1/models" && r.Method == http.MethodGet:
		s.handleModels(w, r)
	case r.URL.Path == "/v1/chat/completions" && r.Method == http.MethodPost:
		s.handleChatCompletions(w, r, key)
	default:
		writeErr(w, http.StatusNotFound, "not found")
	}
}

// handleModels proxies the upstream model list through one account.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	body, status, err := s.withAccount(func(ctx context.Context, apiKey string) (any, int, error) {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		st, b, err := s.upstream.Models(cctx, apiKey)
		return b, st, err
	})
	if err != nil {
		writeErr(w, status, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body.([]byte))
}

// withAccount runs fn against accounts in rotation until one succeeds or the
// pool is exhausted. fn returns (result, upstreamStatus, err).
func (s *Server) withAccount(fn func(ctx context.Context, apiKey string) (any, int, error)) (any, int, error) {
	var lastErr error
	status := http.StatusServiceUnavailable
	tried := map[string]bool{}
	for {
		acc, release, err := s.pool.Acquire()
		if err != nil {
			if lastErr != nil {
				return nil, status, lastErr
			}
			return nil, http.StatusServiceUnavailable, pool.ErrNoAccount
		}
		if tried[acc.ID] {
			release(false, "already tried")
			break
		}
		tried[acc.ID] = true
		res, st, ferr := fn(context.Background(), acc.ApiKey)
		status = st
		if ferr != nil {
			release(false, ferr.Error())
			lastErr = ferr
			continue
		}
		if upstream.IsAuthError(st) {
			release(false, "upstream auth rejected")
			s.pool.MarkAuthError(acc.ID, "upstream rejected api key")
			lastErr = ferr
			if lastErr == nil {
				lastErr = errAuthRejected
			}
			continue
		}
		if upstream.IsRetryable(st) {
			release(false, "upstream retryable status")
			lastErr = errUpstreamBusy
			continue
		}
		if st < 200 || st >= 300 {
			release(false, "upstream error status")
			return nil, st, errUpstreamError
		}
		release(true, "")
		return res, st, nil
	}
	return nil, status, lastErr
}

var (
	errAuthRejected = errString("upstream rejected api key")
	errUpstreamBusy = errString("upstream busy, please retry")
	errUpstreamError = errString("upstream error")
)

type errString string

func (e errString) Error() string { return string(e) }

// handleChatCompletions proxies chat requests: SSE passthrough when
// stream=true, aggregated single response otherwise. Usage from the upstream
// response is recorded per (day, key, account, model).
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request, key *store.ApiKey) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(s.cfg.MaxRequestBytes)))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body failed")
		return
	}
	stream := peekStream(body)
	model := peekModel(body)
	keyID := ""
	if key != nil {
		keyID = key.ID
	}
	// 流式请求向上游要 usage（记账用），客户端未带也补上。
	if stream {
		body = ensureStreamUsage(body)
	}

	tried := map[string]bool{}
	for {
		acc, release, err := s.pool.Acquire()
		if err != nil {
			writeErr(w, http.StatusServiceUnavailable, "no eligible account")
			return
		}
		if tried[acc.ID] {
			release(false, "already tried")
			writeErr(w, http.StatusBadGateway, "all accounts failed")
			return
		}
		tried[acc.ID] = true

		ctx, cancel := context.WithTimeout(r.Context(), s.cfg.UpstreamTimeout)
		resp, derr := s.upstream.ChatDo(ctx, acc.ApiKey, body)
		if derr != nil {
			cancel()
			release(false, derr.Error())
			continue // try next account
		}
		if upstream.IsAuthError(resp.StatusCode) {
			resp.Body.Close()
			cancel()
			release(false, "upstream auth rejected")
			s.pool.MarkAuthError(acc.ID, "upstream rejected api key")
			continue
		}
		if upstream.IsRetryable(resp.StatusCode) {
			resp.Body.Close()
			cancel()
			release(false, "upstream retryable status")
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			resp.Body.Close()
			cancel()
			release(false, "upstream status "+resp.Status)
			writeErr(w, http.StatusBadGateway, "upstream error: "+strings.TrimSpace(string(msg)))
			return
		}
		// Success: proxy the response and record usage.
		release(true, "")
		if stream {
			sc := newSSEUsageScanner()
			proxySSE(w, resp, sc)
			if u := sc.currentUsage(); u != nil {
				s.db.RecordUsage(keyID, acc.ID, model, u.PromptTokens, u.CompletionTokens)
			} else {
				// 无 usage 信息时至少记一次请求。
				s.db.RecordUsage(keyID, acc.ID, model, 0, 0)
			}
		} else {
			u := proxySingle(w, resp)
			s.db.RecordUsage(keyID, acc.ID, model, u.PromptTokens, u.CompletionTokens)
		}
		cancel()
		return
	}
}

// chatUsage is the OpenAI usage object we extract for accounting.
type chatUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// peekModel extracts the request model name for accounting.
func peekModel(body []byte) string {
	var v struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &v)
	return v.Model
}

// ensureStreamUsage injects stream_options.include_usage so streaming
// responses carry a final usage chunk (upstream supports it).
func ensureStreamUsage(body []byte) []byte {
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		return body
	}
	opts, ok := v["stream_options"].(map[string]any)
	if !ok {
		opts = map[string]any{}
	}
	if inc, ok := opts["include_usage"].(bool); ok && inc {
		return body
	}
	opts["include_usage"] = true
	v["stream_options"] = opts
	out, err := json.Marshal(v)
	if err != nil {
		return body
	}
	return out
}

// peekStream reports whether the request asks for SSE streaming.
func peekStream(body []byte) bool {
	var v struct {
		Stream *bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return false
	}
	return v.Stream != nil && *v.Stream
}

// sseUsageScanner watches a byte stream for SSE data lines carrying usage.
type sseUsageScanner struct {
	buf   bytes.Buffer
	usage *chatUsage
}

func newSSEUsageScanner() *sseUsageScanner { return &sseUsageScanner{} }

// Write consumes proxied bytes and parses complete data lines.
func (s *sseUsageScanner) Write(p []byte) (int, error) {
	s.buf.Write(p)
	for {
		line, err := s.buf.ReadString('\n')
		if err != nil {
			// ReadString 已把这段从缓冲消费，读不到换行说明是不完整行，放回等待下一个 chunk。
			s.buf.WriteString(line)
			break
		}
		s.consumeLine(strings.TrimRight(line, "\r\n"))
	}
	return len(p), nil
}

// consumeLine parses one complete SSE line.
func (s *sseUsageScanner) consumeLine(line string) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" || payload == "[DONE]" {
		return
	}
	var chunk struct {
		Usage *chatUsage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return
	}
	if chunk.Usage != nil {
		s.usage = chunk.Usage
	}
}

// currentUsage returns the last seen usage, if any.
func (s *sseUsageScanner) currentUsage() *chatUsage { return s.usage }

// proxySSE copies the upstream SSE stream to the client verbatim, optionally
// feeding every byte to an observer (usage scanner).
func proxySSE(w http.ResponseWriter, resp *http.Response, obs io.Writer) {
	defer resp.Body.Close()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)
	fl, ok := w.(http.Flusher)
	if ok {
		fl.Flush()
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
			if ok {
				fl.Flush()
			}
			if obs != nil {
				_, _ = obs.Write(buf[:n])
			}
		}
		if err != nil {
			break
		}
	}
}

// proxySingle aggregates a non-streaming upstream response and returns its
// usage for accounting.
func proxySingle(w http.ResponseWriter, resp *http.Response) *chatUsage {
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		writeErr(w, http.StatusBadGateway, "read upstream failed")
		return nil
	}
	if !json.Valid(body) {
		body = bytes.TrimSpace(body)
	}
	var v struct {
		Usage *chatUsage `json:"usage"`
	}
	_ = json.Unmarshal(body, &v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
	if v.Usage == nil {
		return &chatUsage{}
	}
	return v.Usage
}
