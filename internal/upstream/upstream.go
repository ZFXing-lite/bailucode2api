// Package upstream is the thin HTTP client for the bailucode OpenAPI.
// All authentication details live here so the TODO(bailu-console) items are
// easy to find and fix after the console reconnaissance.
package upstream

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"time"
)

// authHeader builds the upstream Authorization header.
func authHeader(apiKey string) (name, value string) {
	return "Authorization", "Bearer " + strings.TrimSpace(apiKey)
}

// Client talks to https://bailucode.com/openapi/v1 (configurable).
type Client struct {
	base   string
	http   *http.Client
	modelsPath string
	chatPath   string
}

func New(base string, timeout time.Duration) *Client {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		base = "https://bailucode.com/openapi/v1"
	}
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &Client{
		base: base,
		http: &http.Client{Timeout: timeout},
		// NOTE: base 已包含 /v1（https://bailucode.com/openapi/v1），
		// 因此端点路径不再重复加 /v1 前缀。
		modelsPath: "/models",
		chatPath:   "/chat/completions",
	}
}

// newRequest builds an authenticated upstream request.
func (c *Client) newRequest(ctx context.Context, method, path, apiKey string, body []byte) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	name, value := authHeader(apiKey)
	req.Header.Set(name, value)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return req, nil
}

func readBody(resp *http.Response, limit int64) ([]byte, error) {
	defer resp.Body.Close()
	if limit <= 0 {
		limit = 4 << 20
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// Models fetches the upstream model list (lightweight authenticated GET).
// The keepalive worker also uses it as the auth ping.
func (c *Client) Models(ctx context.Context, apiKey string) (int, []byte, error) {
	req, err := c.newRequest(ctx, http.MethodGet, c.modelsPath, apiKey, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	body, err := readBody(resp, 1<<20)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// ChatDo sends a chat completion request and returns the raw response so the
// gateway can stream it (SSE) or aggregate it. The caller must close resp.Body.
func (c *Client) ChatDo(ctx context.Context, apiKey string, body []byte) (*http.Response, error) {
	req, err := c.newRequest(ctx, http.MethodPost, c.chatPath, apiKey, body)
	if err != nil {
		return nil, err
	}
	return c.http.Do(req)
}

// IsAuthError reports whether an upstream status code means the API key is
// bad/expired (as opposed to a transient failure).
func IsAuthError(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

// IsRetryable reports whether the failure is worth retrying on another account.
func IsRetryable(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}
