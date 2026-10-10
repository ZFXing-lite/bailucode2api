// Package bailu is the HTTP client for the bailucode control panel
// (https://bailucode.com/api_platform/).
//
// 职责：账号登录、状态查询（get_stats）、每日额度领取。
// 请求需要携带 X-Request-* 签名头，构造方式见 signHeaders。
package bailu

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const sigVersion = "authReqSig20260308d"

// Common errors.
var (
	ErrBadCredentials = errors.New("bailu: 邮箱或密码无效")
	ErrBehaviorReject = errors.New("bailu: 行为验证未通过")
	ErrRateLimited    = errors.New("bailu: 操作过于频繁")
	ErrSessionExpired = errors.New("bailu: session 已失效")
)

// Stats is the /api/api_keys get_stats payload (fields we care about).
// tokens_*_effective 为含定向包的生效值。
type Stats struct {
	ActiveKeys           int            `json:"active_keys"`
	Requests30d          int            `json:"requests_30d"`
	TokensUsed           float64        `json:"tokens_used_effective"`
	TokensQuota          float64        `json:"tokens_quota_effective"`
	AddonTokensRemaining float64        `json:"addon_tokens_remaining"`
	AddonTokensConsumed  float64        `json:"addon_tokens_consumed"`
	RateLimit            int            `json:"api_rate_limit"`
	TPMLimit             int            `json:"api_tpm_limit"`
	ConcurrentLimit      int            `json:"api_concurrent_limit"`
	CreditsUsed          float64        `json:"credits_used"`
	CreditsQuota         float64        `json:"credits_quota"`
	AnthropicPromoClaimed bool          `json:"anthropic_promo_claimed"`
	OpenclawPromoClaimed  bool          `json:"openclaw_promo_claimed"`
	FestivalPromoOpen     bool          `json:"festival_promo_open"`
	FestivalPromoClaimed  bool          `json:"festival_promo_claimed"`
	PromoHumanVerified    bool          `json:"promo_human_verified"`
	DirectedPacks         []DirectedPack `json:"directed_packs"`
}

// DirectedPack is a定向额度包（如 Anthropic 接入定向）.
type DirectedPack struct {
	ID    string  `json:"id"`
	Label string  `json:"label"`
	Used  float64 `json:"used"`
	Quota float64 `json:"quota"`
	Pct   float64 `json:"pct"`
}

// ClaimResponse is the outcome of a claim action.
type ClaimResponse struct {
	Success        bool    `json:"success"`
	AlreadyClaimed bool    `json:"already_claimed"`
	TokensAdded    float64 `json:"tokens_added"`
	Message        string  `json:"message"`
}

// Client holds one authenticated session against the control panel.
type Client struct {
	base    string
	hc      *http.Client
	cookies map[string]string
	fp      string
}

func New(base string, timeout time.Duration) *Client {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		base = "https://bailucode.com"
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &Client{base: base, hc: &http.Client{Timeout: timeout}, cookies: map[string]string{}}
}

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36"

// cookieHeader renders the session jar as a Cookie header value.
func (c *Client) cookieHeader() string {
	var b strings.Builder
	for k, v := range c.cookies {
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		b.WriteString(k + "=" + v)
	}
	return b.String()
}

func (c *Client) absorbCookies(resp *http.Response) {
	for _, line := range resp.Header.Values("Set-Cookie") {
		kv := strings.SplitN(line, ";", 2)[0]
		if eq := strings.IndexByte(kv, '='); eq > 0 {
			c.cookies[strings.TrimSpace(kv[:eq])] = strings.TrimSpace(kv[eq+1:])
		}
	}
}

// signHeaders builds the four X-Request-* headers for a signed request.
// Must match the browser's auth-app.js exactly.
func (c *Client) signHeaders(path string, body []byte) map[string]string {
	if c.fp == "" {
		return nil
	}
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	rawNonce := make([]byte, 8)
	_, _ = rand.Read(rawNonce)
	nh := sha256.Sum256([]byte(ts + "." + hex.EncodeToString(rawNonce)))
	nonce := hex.EncodeToString(nh[:])[:32]
	msg := strings.Join([]string{
		"POST", path, string(body), ts, nonce, c.fp, sigVersion,
	}, "\n")
	mac := hmac.New(sha256.New, []byte(c.fp))
	mac.Write([]byte(msg))
	return map[string]string{
		"X-Request-Timestamp":  ts,
		"X-Request-Nonce":      nonce,
		"X-Request-Signature":  hex.EncodeToString(mac.Sum(nil)),
		"X-Client-Fingerprint": c.fp,
	}
}

// post sends a signed JSON POST and decodes the JSON reply.
func (c *Client) post(ctx context.Context, path string, body any, out any) (int, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Origin", c.base)
	req.Header.Set("Referer", c.base+"/auth/login?return_url=%2Fapi_platform%2F")
	if ck := c.cookieHeader(); ck != "" {
		req.Header.Set("Cookie", ck)
	}
	for k, v := range c.signHeaders(path, raw) {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	c.absorbCookies(resp)
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("bailu: non-json reply (%d): %.120s", resp.StatusCode, string(data))
		}
	}
	return resp.StatusCode, nil
}

// bootstrapResp is the bootstrap.php payload.
type bootstrapResp struct {
	Success bool `json:"success"`
	Message string `json:"message"`
	Data struct {
		BootstrapToken    string `json:"bootstrap_token"`
		RequestID         string `json:"request_id"`
		Fp                string `json:"fp"`
		ChallengeRequired bool   `json:"challenge_required"`
		Pow               *struct {
			Challenge  string `json:"challenge"`
			Difficulty int    `json:"difficulty"`
		} `json:"pow"`
	} `json:"data"`
}

type loginResp struct {
	Success      bool   `json:"success"`
	Message      string `json:"message"`
	SessionToken string `json:"session_token"`
}

// Login performs the full password flow: bootstrap → (PoW) → signed login.
func (c *Client) Login(ctx context.Context, email, password string) error {
	c.cookies = map[string]string{} // fresh session

	// Touch the login page to obtain PHPSESSID.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/auth/login?return_url=%2Fapi_platform%2F", nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	c.absorbCookies(resp)

	behavior := ForgeBehavior(email, password)

	var boot bootstrapResp
	if _, err := c.post(ctx, "/auth/api/bootstrap.php", map[string]any{
		"flow":    "login_password",
		"payload": map[string]any{"email": email},
		"behavior": behavior,
	}, &boot); err != nil {
		return err
	}
	if !boot.Success {
		return fmt.Errorf("bailu: bootstrap: %s", boot.Message)
	}
	c.fp = boot.Data.Fp

	var challenge map[string]any
	if boot.Data.ChallengeRequired && boot.Data.Pow != nil {
		solved, err := solvePoW(boot.Data.Pow.Challenge, boot.Data.Pow.Difficulty)
		if err != nil {
			return err
		}
		challenge = map[string]any{"nonce": solved.nonce, "hash": solved.hash}
	}

	var login loginResp
	if _, err := c.post(ctx, "/auth/api/login.php", map[string]any{
		"mode":           "password",
		"email":          email,
		"password":       password,
		"bootstrap_token": boot.Data.BootstrapToken,
		"behavior":       behavior,
		"challenge":      challenge,
	}, &login); err != nil {
		return err
	}
	if !login.Success {
		switch {
		case strings.Contains(login.Message, "邮箱或密码无效"):
			return ErrBadCredentials
		case strings.Contains(login.Message, "行为验证未通过"):
			return ErrBehaviorReject
		case strings.Contains(login.Message, "频繁"):
			return ErrRateLimited
		}
		return fmt.Errorf("bailu: login: %s", login.Message)
	}
	// session_token must ride along as a cookie for /api_platform requests.
	if login.SessionToken != "" {
		c.cookies["session_token"] = login.SessionToken
	}
	return nil
}

type statsResp struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Stats   *Stats `json:"stats"`
}

// Stats fetches the account state (claim status, quotas).
func (c *Client) Stats(ctx context.Context) (*Stats, error) {
	var r statsResp
	if _, err := c.post(ctx, "/api/api_keys", map[string]any{"action": "get_stats"}, &r); err != nil {
		return nil, err
	}
	if !r.Success || r.Stats == nil {
		if strings.Contains(r.Message, "session") || strings.Contains(r.Message, "登录") {
			return nil, ErrSessionExpired
		}
		return nil, fmt.Errorf("bailu: get_stats: %s", r.Message)
	}
	return r.Stats, nil
}

// ClaimSlot performs one promo claim. turnstileToken may be empty when the
// session is already promo_human_verified (second slot of the day).
func (c *Client) ClaimSlot(ctx context.Context, slot, turnstileToken string) (*ClaimResponse, error) {
	action := "claim_anthropic_promo"
	if slot == "slot_b" {
		action = "claim_openclaw_promo"
	}
	var r ClaimResponse
	status, err := c.post(ctx, "/api/api_keys", map[string]any{
		"action":        action,
		"turnstileToken": turnstileToken,
	}, &r)
	if err != nil {
		return nil, err
	}
	if status == http.StatusBadRequest && strings.Contains(r.Message, "人机验证") {
		return &r, fmt.Errorf("bailu: claim rejected: %s", r.Message)
	}
	return &r, nil
}

// solvePoW finds a nonce so sha256(challenge:nonce-hex) has `difficulty` leading zeros.
// The browser pow.js uses nonce.toString(16); Go mirrors that.
type powSolution struct{ nonce, hash string }

func solvePoW(challenge string, difficulty int) (powSolution, error) {
	if difficulty <= 0 || difficulty > 8 {
		difficulty = 3
	}
	prefix := strings.Repeat("0", difficulty)
	deadline := time.Now().Add(60 * time.Second)
	for n := 0; n < 3_000_000; n++ {
		nonce := strconv.FormatInt(int64(n), 16)
		sum := sha256.Sum256([]byte(challenge + ":" + nonce))
		hexh := hex.EncodeToString(sum[:])
		if strings.HasPrefix(hexh, prefix) {
			return powSolution{nonce: nonce, hash: hexh}, nil
		}
		if n%100000 == 0 && time.Now().After(deadline) {
			break
		}
	}
	return powSolution{}, errors.New("bailu: pow exhausted")
}

// randHex returns n random hex chars.
func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

// randRange returns a pseudo-random int in [lo, hi].
func randRange(lo, hi int) int {
	if hi <= lo {
		return lo
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(hi-lo+1)))
	if err != nil {
		return lo
	}
	return lo + int(n.Int64())
}

var _ = randHex // keep for future use
