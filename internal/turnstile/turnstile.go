// Package turnstile obtains Cloudflare Turnstile tokens for the promo claim.
//
// 签到领取需要通过 Cloudflare Turnstile 验证。验证对浏览器运行环境有
// 真实性要求：需要真实显示环境（Xvfb 提供虚拟屏幕）与浏览器原生指纹，
// 详见 scripts/claim_helper.js。
//
// 求解器选择（TURNSTILE_SOLVER）：
//   browser  — 调 scripts/claim_helper.js（需 Chrome + Node + puppeteer-core + Xvfb）
//   capsolver — 付费 API（CAPSOLVER_KEY），适合无浏览器的服务器
//   manual   — 不求解，签到仅报告"需人工领取"
package turnstile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// SiteKey is the Turnstile sitekey used by the promo claim page.
const SiteKey = "0x4AAAAAAB4TbrXJmPX8Qu8W"

// Solver yields a Turnstile token for the given page URL.
type Solver interface {
	Token(ctx context.Context, pageURL string) (string, error)
	Name() string
}

// ---------------------------------------------------------------- capsolver

type Capsolver struct {
	APIKey string
	Client *http.Client
}

func NewCapsolver(key string) *Capsolver {
	return &Capsolver{APIKey: key, Client: &http.Client{Timeout: 120 * time.Second}}
}

func (c *Capsolver) Name() string { return "capsolver" }

func (c *Capsolver) Token(ctx context.Context, pageURL string) (string, error) {
	create := map[string]any{
		"clientKey": c.APIKey,
		"task": map[string]any{
			"type":       "AntiTurnstileTaskProxyLess",
			"websiteURL": pageURL,
			"websiteKey": SiteKey,
			"metadata":   map[string]any{"action": "", "cdata": ""},
		},
	}
	var created struct {
		ErrorID          int    `json:"errorId"`
		ErrorCode        string `json:"errorCode"`
		TaskID           string `json:"taskId"`
	}
	if err := c.do(ctx, "https://api.capsolver.com/createTask", create, &created); err != nil {
		return "", err
	}
	if created.ErrorID != 0 || created.TaskID == "" {
		return "", fmt.Errorf("capsolver createTask: %s", created.ErrorCode)
	}
	for i := 0; i < 90; i++ {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
		var result struct {
			ErrorID  int    `json:"errorId"`
			ErrorCode string `json:"errorCode"`
			Status   string `json:"status"`
			Solution struct {
				Token string `json:"token"`
			} `json:"solution"`
		}
		if err := c.do(ctx, "https://api.capsolver.com/getTaskResult", map[string]any{
			"clientKey": c.APIKey, "taskId": created.TaskID,
		}, &result); err != nil {
			return "", err
		}
		if result.Status == "ready" && result.Solution.Token != "" {
			return result.Solution.Token, nil
		}
		if result.ErrorID != 0 {
			return "", fmt.Errorf("capsolver: %s", result.ErrorCode)
		}
	}
	return "", errors.New("capsolver: timeout")
}

func (c *Capsolver) do(ctx context.Context, url string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// ---------------------------------------------------------------- browser

// Browser runs the bundled Node helper: a headful (Xvfb) Chrome that logs in,
// passes Turnstile and claims both promo slots end-to-end. Here we only need
// the token part — but running the full helper once covers both slots in one
// browser session, so Token is not used by the browser solver; the checkin
// package drives the helper directly.
type Browser struct {
	NodePath  string // "node"
	ScriptPath string // absolute path to scripts/claim_helper.js
	ChromePath string
	WorkDir   string
}

func NewBrowser(scriptPath, chromePath, workDir string) *Browser {
	return &Browser{NodePath: "node", ScriptPath: scriptPath, ChromePath: chromePath, WorkDir: workDir}
}

func (b *Browser) Name() string { return "browser" }

// Token is implemented for interface completeness; the browser flow solves
// and claims in one pass (see checkin), so this returns a helpful error.
func (b *Browser) Token(ctx context.Context, pageURL string) (string, error) {
	return "", errors.New("browser solver claims in one pass; use checkin's helper run")
}

// ClaimRequest is the JSON fed to claim_helper.js on stdin.
type ClaimRequest struct {
	Email      string `json:"email"`
	Password   string `json:"password"`
	Origin     string `json:"origin"`
	ChromePath string `json:"chromePath"`
}

// ClaimResult is the JSON claim_helper.js prints on stdout.
type ClaimResult struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	HumanV  bool   `json:"human_verified"`
	Results []struct {
		Slot    string `json:"slot"`
		Claimed bool   `json:"claimed"`
		Already bool   `json:"already"`
		Error   string `json:"error,omitempty"`
	} `json:"results"`
	Stats map[string]any `json:"stats,omitempty"`
}

// RunClaim executes the helper for one account and parses its JSON result.
func (b *Browser) RunClaim(ctx context.Context, email, password, origin string) (*ClaimResult, error) {
	req := ClaimRequest{Email: email, Password: password, Origin: origin, ChromePath: b.ChromePath}
	raw, _ := json.Marshal(req)

	node := b.NodePath
	if node == "" {
		node = "node"
	}
	// xvfb-run 包装优先：无显示器环境下 headful Chrome 需要 Xvfb。
	argv := []string{node, b.ScriptPath}
	if os.Getenv("DISPLAY") == "" {
		if _, err := exec.LookPath("xvfb-run"); err == nil {
			argv = append([]string{"xvfb-run", "-a", "-s", "-screen 0 1366x900x24"}, argv...)
		}
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(raw)
	cmd.Stdout = &syncBuffer{}
	cmd.Stderr = &syncBuffer{}
	if b.WorkDir != "" {
		cmd.Dir = b.WorkDir
	}
	var out, errb syncBuffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("helper run: %v; stderr: %s", err, strings.TrimSpace(errb.String()))
	}
	line := strings.TrimSpace(out.String())
	// helper 可能在 JSON 前打日志行；取最后一个以 { 开头的行
	if idx := strings.LastIndex(line, "\n{"); idx >= 0 {
		line = strings.TrimSpace(line[idx+1:])
	}
	var res ClaimResult
	if !strings.HasPrefix(line, "{") {
		return nil, fmt.Errorf("helper output not json: %.200s", line)
	}
	if err := json.Unmarshal([]byte(line), &res); err != nil {
		return nil, fmt.Errorf("helper json decode: %v", err)
	}
	return &res, nil
}

// syncBuffer is a tiny threadsafe-free buffer (single reader).
type syncBuffer struct{ buf bytes.Buffer }

func (s *syncBuffer) Write(p []byte) (int, error) { return s.buf.Write(p) }
func (s *syncBuffer) String() string              { return s.buf.String() }

// ---------------------------------------------------------------- manual

type Manual struct{}

func (Manual) Name() string { return "manual" }
func (Manual) Token(_ context.Context, _ string) (string, error) {
	return "", errors.New("需人工在控制台完成 Turnstile 验证后领取")
}
