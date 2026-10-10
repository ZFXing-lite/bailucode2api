// Package checkin runs the daily sign-in scheduler (08:00 and 20:00
// Asia/Shanghai) that claims quota for every account at the two sign-in
// spots:
//
//	SlotA = Anthropic 定向额度（claim_anthropic_promo）
//	SlotB = API 总额度（claim_openclaw_promo）
//
// 流程：先查询当日领取状态，已领取跳过；未领取一侧通过配置的
// Turnstile 求解器（browser/capsolver/manual）完成验证后领取，
// 幂等设计，随时可安全重试。
package checkin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"bailucode2api/internal/bailu"
	"bailucode2api/internal/store"
	"bailucode2api/internal/turnstile"
)

// Sign-in slots.
const (
	SlotA = "slot_a" // Anthropic 定向领取
	SlotB = "slot_b" // API 总额度领取
)

var slotNames = map[string]string{
	SlotA: "Anthropic 定向",
	SlotB: "API 总额度",
}

func allSlots() []string { return []string{SlotA, SlotB} }

// Settings carries the solver wiring resolved from config.
type Settings struct {
	PanelBase  string // https://bailucode.com
	SolverKind string // browser | capsolver | manual
	Capsolver  *turnstile.Capsolver
	Browser    *turnstile.Browser
}

// CheckinResult is the per-slot outcome for one account.
type CheckinResult struct {
	Slot        string `json:"slot"`
	SlotName    string `json:"slot_name"`
	AlreadyDone bool   `json:"already_done"`
	Claimed     bool   `json:"claimed"`
	Skipped     bool   `json:"skipped"` // solver unavailable → manual needed
	Error       string `json:"error,omitempty"`
	QuotaAfter  float64 `json:"quota_after,omitempty"`
}

func result(slot string) CheckinResult {
	return CheckinResult{Slot: slot, SlotName: slotNames[slot]}
}

// helperRunResult captures one browser-helper pass (both slots in one go).
type helperRunResult struct {
	touched bool
	results []CheckinResult
	stats   *bailu.Stats
}

// runBrowserHelper runs scripts/claim_helper.js once for the account.
func runBrowserHelper(ctx context.Context, s Settings, a *store.Account) (*helperRunResult, error) {
	if s.Browser == nil {
		return nil, errors.New("browser solver not configured")
	}
	hres, err := s.Browser.RunClaim(ctx, a.Email, a.Password, s.PanelBase)
	if err != nil {
		return nil, err
	}
	out := &helperRunResult{touched: hres.OK}
	for _, r := range hres.Results {
		cr := result(r.Slot)
		switch {
		case r.Already:
			cr.AlreadyDone = true
		case r.Claimed:
			cr.Claimed = true
		default:
			cr.Error = orDefault(r.Error, "领取失败")
		}
		out.results = append(out.results, cr)
	}
	if hres.Error != "" && !hres.OK && len(out.results) == 0 {
		return nil, errors.New(hres.Error)
	}
	// 携带 helper 看到的额度快照
	if hres.Stats != nil {
		raw, _ := json.Marshal(hres.Stats)
		var st bailu.Stats
		_ = json.Unmarshal(raw, &st)
		out.stats = &st
	}
	return out, nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// claimViaHTTP implements the capsolver/manual paths over the pure-HTTP client.
func claimViaHTTP(ctx context.Context, s Settings, a *store.Account) ([]CheckinResult, *bailu.Stats, error) {
	client := bailu.New(s.PanelBase, 60*time.Second)
	if err := client.Login(ctx, a.Email, a.Password); err != nil {
		return nil, nil, fmt.Errorf("登录: %w", err)
	}
	stats, err := client.Stats(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("状态查询: %w", err)
	}
	var results []CheckinResult
	for _, slot := range allSlots() {
		cr := result(slot)
		claimed := slot == SlotA && stats.AnthropicPromoClaimed
		if slot == SlotB {
			claimed = stats.OpenclawPromoClaimed
		}
		if claimed {
			cr.AlreadyDone = true
			results = append(results, cr)
			continue
		}
		// 本会话未有人机验证时需要 token；capsolver 之外的 solver 视为 manual。
		token := ""
		if !stats.PromoHumanVerified {
			switch {
			case s.Capsolver != nil:
				pageURL := s.PanelBase + "/api_platform/"
				t, terr := s.Capsolver.Token(ctx, pageURL)
				if terr != nil {
					cr.Skipped = true
					cr.Error = "turnstile 求解失败: " + terr.Error()
					results = append(results, cr)
					continue
				}
				token = t
			default:
				cr.Skipped = true
				cr.Error = "TURNSTILE_SOLVER 未配置（browser/capsolver），需人工领取"
				results = append(results, cr)
				continue
			}
		}
		cres, cerr := client.ClaimSlot(ctx, slot, token)
		if cerr != nil && cres == nil {
			cr.Error = cerr.Error()
			results = append(results, cr)
			continue
		}
		if cres != nil && cres.Success {
			cr.Claimed = true
			cr.QuotaAfter = cres.TokensAdded
		} else if cres != nil && cres.AlreadyClaimed {
			cr.AlreadyDone = true
		} else if cres != nil {
			cr.Error = orDefault(cres.Message, "领取失败")
		}
		results = append(results, cr)
	}
	return results, stats, nil
}

// Checkin runs both sign-in slots for one account, idempotently.
func Checkin(ctx context.Context, s Settings, a *store.Account) (allDone bool, results []CheckinResult, err error) {
	if !a.HasApiKey() && a.Password == "" {
		return false, []CheckinResult{{Slot: "both", SlotName: "全部", Skipped: true, Error: "账号缺密码，无法登录控制台"}}, nil
	}
	switch s.SolverKind {
	case "browser":
		h, herr := runBrowserHelper(ctx, s, a)
		if herr != nil {
			return false, []CheckinResult{{Slot: "both", SlotName: "全部", Error: herr.Error()}}, nil
		}
		results = h.results
		// helper 后再查一次状态，刷新额度并确认
		client := bailu.New(s.PanelBase, 60*time.Second)
		if lerr := client.Login(ctx, a.Email, a.Password); lerr == nil {
			if st, serr := client.Stats(ctx); serr == nil {
				h.stats = st
			}
		}
		applyQuota(a, h.stats)
		allDone = true
		for _, r := range results {
			if !r.Claimed && !r.AlreadyDone {
				allDone = false
			}
		}
		return allDone, results, nil
	default: // capsolver | manual
		rs, stats, cerr := claimViaHTTP(ctx, s, a)
		if cerr != nil {
			return false, []CheckinResult{{Slot: "both", SlotName: "全部", Error: cerr.Error()}}, nil
		}
		applyQuota(a, stats)
		allDone = true
		for _, r := range rs {
			if !r.Claimed && !r.AlreadyDone {
				allDone = false
			}
		}
		return allDone, rs, nil
	}
}

// applyQuota snapshots the observed quota onto the account.
func applyQuota(a *store.Account, st *bailu.Stats) {
	if a == nil || st == nil {
		return
	}
	a.Quota = bailu.SnapshotFromStats(st)
}

// Scheduler fires Checkin for every account at 08:00 and 20:00 Asia/Shanghai.
type Scheduler struct {
	db      *store.DB
	enabled bool
	loc     *time.Location
	settings Settings
}

func NewScheduler(db *store.DB, enabled bool, s Settings) *Scheduler {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		log.Printf("checkin: cannot load Asia/Shanghai tz, falling back to UTC: %v", err)
		loc = time.UTC
	}
	return &Scheduler{db: db, enabled: enabled, loc: loc, settings: s}
}

// nextRun returns the next 08:00 / 20:00 instant in the scheduler timezone.
func (s *Scheduler) nextRun(now time.Time) time.Time {
	inLoc := now.In(s.loc)
	y, m, d := inLoc.Date()
	for _, h := range []int{8, 20} {
		t := time.Date(y, m, d, h, 0, 0, 0, s.loc)
		if t.After(inLoc) {
			return t
		}
	}
	return time.Date(y, m, d+1, 8, 0, 0, 0, s.loc)
}

// Start runs the scheduler loop until ctx is done.
func (s *Scheduler) Start(ctx context.Context) {
	if !s.enabled {
		log.Printf("checkin: scheduler disabled (CHECKIN_ENABLED=0)")
		return
	}
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
			s.runAll(ctx, "boot")
		}
		for {
			wait := time.Until(s.nextRun(time.Now()))
			if wait < 0 {
				wait = time.Minute
			}
			log.Printf("checkin: next run in %s (solver=%s)", wait.Round(time.Second), s.settings.SolverKind)
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
				s.runAll(ctx, "scheduled")
			}
		}
	}()
}

// RunOnce triggers an immediate pass (used by the admin API / manual button).
func (s *Scheduler) RunOnce(ctx context.Context) { s.runAll(ctx, "manual") }

func (s *Scheduler) runAll(ctx context.Context, why string) {
	accounts := s.db.List()
	if len(accounts) == 0 {
		log.Printf("checkin: %s run, no accounts", why)
		return
	}
	log.Printf("checkin: %s run for %d accounts (solver=%s)", why, len(accounts), s.settings.SolverKind)
	done, skipped, failed := 0, 0, 0
	for _, a := range accounts {
		if a == nil || a.Disabled || (a.Password == "" && !a.HasApiKey()) {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 3*time.Minute) // browser 求解较慢
		_, results, _ := Checkin(cctx, s.settings, a)
		cancel()
		accSkipped := false
		anyTouched := false
		for _, r := range results {
			switch {
			case r.Skipped:
				accSkipped = true
			case r.Error != "":
				failed++
			case r.AlreadyDone:
				done++
			case r.Claimed:
				done++
				anyTouched = true
			}
		}
		if accSkipped {
			skipped++
		}
		now := time.Now()
		changed := false
		if a.LastCheckin == nil {
			a.LastCheckin = map[string]time.Time{}
		}
		for _, r := range results {
			if r.Claimed || (r.AlreadyDone && !isDoneToday(a.LastCheckin[r.Slot], now)) {
				a.LastCheckin[r.Slot] = now
				changed = true
			}
		}
		if a.Quota != nil {
			changed = true
		}
		if changed || anyTouched {
			_ = s.db.Upsert(a)
		}
	}
	log.Printf("checkin: %s run finished ok=%d skipped(需人工)=%d failed=%d", why, done, skipped, failed)
}

func isDoneToday(t time.Time, now time.Time) bool {
	if t.IsZero() {
		return false
	}
	y1, m1, d1 := t.Local().Date()
	y2, m2, d2 := now.Local().Date()
	return y1 == y2 && m1 == m2 && d1 == d2
}

// DefaultHelperPath locates scripts/claim_helper.js next to the executable.
func DefaultHelperPath() string {
	exe, err := filepath.Abs(".")
	if err == nil {
		p := filepath.Join(exe, "scripts", "claim_helper.js")
		return p
	}
	return "scripts/claim_helper.js"
}
