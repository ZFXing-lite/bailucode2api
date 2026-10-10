// Package pool is the upstream account pool: simple round-robin selection
// with per-account in-flight caps and a lightweight consecutive-failure
// circuit breaker (no weighted scoring — deliberately simpler than the hw
// reference panel).
package pool

import (
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"bailucode2api/internal/store"
)

var ErrNoAccount = errors.New("pool: no eligible account")

// Breaker policy.
const (
	failThreshold   = 3
	baseCooldown    = 5 * time.Minute
	maxCooldown     = 1 * time.Hour
	stickyFailLimit = 10 // hard-disable after this many consecutive failures
)

// Pool picks accounts for upstream calls.
type Pool struct {
	db          *store.DB
	maxInflight int

	mu       sync.Mutex
	rr       atomic.Uint64
	inflight map[string]int
}

func New(db *store.DB, maxInflight int) *Pool {
	if maxInflight <= 0 {
		maxInflight = 4
	}
	return &Pool{db: db, maxInflight: maxInflight, inflight: map[string]int{}}
}

// ReleaseFunc reports the outcome of one upstream call for the account.
type ReleaseFunc func(success bool, errMsg string)

// Acquire picks the next callable account (round-robin) and reserves one
// in-flight slot. The caller MUST call the returned ReleaseFunc.
func (p *Pool) Acquire() (*store.Account, ReleaseFunc, error) {
	now := time.Now()
	cands := p.eligible(now)
	if len(cands) == 0 {
		return nil, nil, ErrNoAccount
	}
	// Round-robin with in-flight cap: walk at most len(cands) slots.
	start := p.rr.Add(1)
	for i := 0; i < len(cands); i++ {
		a := cands[(int(start)+i)%len(cands)]
		p.mu.Lock()
		if p.inflight[a.ID] < p.maxInflight {
			p.inflight[a.ID]++
			p.mu.Unlock()
			return a, p.releaseFunc(a), nil
		}
		p.mu.Unlock()
	}
	return nil, nil, ErrNoAccount
}

func (p *Pool) eligible(now time.Time) []*store.Account {
	var out []*store.Account
	for _, a := range p.db.List() {
		if a.Callable(now) {
			out = append(out, a)
		}
	}
	return out
}

func (p *Pool) releaseFunc(a *store.Account) ReleaseFunc {
	return func(success bool, errMsg string) {
		p.mu.Lock()
		if p.inflight[a.ID] > 0 {
			p.inflight[a.ID]--
		}
		p.mu.Unlock()

		now := time.Now()
		acc, err := p.db.Get(a.ID)
		if err != nil {
			return
		}
		acc.LastUsedAt = now
		if success {
			acc.SuccessCount++
			acc.LastSuccessAt = now
			acc.ConsecutiveFails = 0
			acc.CooldownUntil = time.Time{}
			if acc.LastError != "" {
				acc.LastError = ""
			}
		} else {
			acc.FailCount++
			acc.ConsecutiveFails++
			if errMsg != "" {
				acc.LastError = errMsg
			}
			if acc.ConsecutiveFails >= failThreshold {
				shift := acc.ConsecutiveFails - failThreshold
				if shift > 4 {
					shift = 4
				}
				cd := baseCooldown << shift
				if cd > maxCooldown {
					cd = maxCooldown
				}
				acc.CooldownUntil = now.Add(cd)
				log.Printf("pool: %s cooling down until %s (%d consecutive fails: %s)",
					acc.Email, acc.CooldownUntil.Format(time.RFC3339), acc.ConsecutiveFails, errMsg)
			}
			if acc.ConsecutiveFails >= stickyFailLimit {
				acc.Status = store.StatusError
				acc.DisabledReason = "too many consecutive failures"
				log.Printf("pool: %s marked error after %d consecutive failures", acc.Email, acc.ConsecutiveFails)
			}
		}
		_ = p.db.Upsert(acc)
	}
}

// MarkAuthError hard-marks an account whose API key was rejected upstream.
func (p *Pool) MarkAuthError(id, errMsg string) {
	acc, err := p.db.Get(id)
	if err != nil {
		return
	}
	acc.Status = store.StatusError
	acc.DisabledReason = "upstream rejected api key"
	acc.LastError = errMsg
	acc.UpdatedAt = time.Now()
	_ = p.db.Upsert(acc)
}

// Stats returns a quick pool summary for the dashboard.
func (p *Pool) Stats() map[string]any {
	now := time.Now()
	total, callable := 0, 0
	inflight := 0
	for _, a := range p.db.List() {
		total++
		if a.Callable(now) {
			callable++
		}
	}
	p.mu.Lock()
	for _, n := range p.inflight {
		inflight += n
	}
	p.mu.Unlock()
	return map[string]any{
		"total": total, "callable": callable, "inflight": inflight,
	}
}
