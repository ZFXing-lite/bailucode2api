// Package store persists bailucode upstream accounts to a local JSON file.
// Sensitive fields (password, apikey) are AES-GCM encrypted at rest and the
// file is written with 0600 permissions. Adapted from the hw reference panel,
// trimmed down to what bailucode2api needs.
package store

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Account statuses.
const (
	StatusActive     = "active"      // usable
	StatusPendingKey = "pending_key" // imported without apikey yet
	StatusDisabled   = "disabled"    // manually disabled by admin
	StatusError      = "error"       // upstream rejected / auth failed
)

// QuotaSnapshot caches the last observed control-panel quota for an account.
// 数据源：控制台 POST /api/api_keys {action:"get_stats"}。
// Tokens 字段为含定向包的生效值；Credits 为美元额度；Packs 为定向额度包。
type QuotaSnapshot struct {
	UsedTokens   float64     `json:"used_tokens,omitempty"`
	TotalTokens  float64     `json:"total_tokens,omitempty"`
	RemainTokens float64     `json:"remain_tokens,omitempty"`
	CreditsUsed  float64     `json:"credits_used,omitempty"`
	CreditsQuota float64     `json:"credits_quota,omitempty"`
	Packs        []PackQuota `json:"packs,omitempty"`
	Raw          string      `json:"raw,omitempty"` // raw upstream payload for debugging
	FetchedAt    time.Time   `json:"fetched_at"`
	Error        string      `json:"error,omitempty"`
}

// PackQuota is one directed quota pack (e.g. Anthropic 接入定向).
type PackQuota struct {
	ID    string  `json:"id"`
	Label string  `json:"label"`
	Used  float64 `json:"used"`
	Quota float64 `json:"quota"`
	Pct   float64 `json:"pct"`
}

// ApiKey is a downstream caller key managed from the admin panel.
// Secret is AES-GCM encrypted at rest; Prefix keeps a short plaintext
// hint for display.
type ApiKey struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Secret    string    `json:"secret,omitempty"` // encrypted at rest
	Prefix    string    `json:"prefix"`           // e.g. "sk-b2a-Ab12…"
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

// DailyUsage is the per-day usage rollup, keyed by (date,key,account,model).
type DailyUsage struct {
	Date      string `json:"date"` // 2006-01-02
	KeyID     string `json:"key_id"`
	AccountID string `json:"account_id"`
	Model     string `json:"model"`
	Requests  int64  `json:"requests"`
	TokensIn  int64  `json:"tokens_in"`
	TokensOut int64  `json:"tokens_out"`
}

func (u *DailyUsage) tokensTotal() int64 { return u.TokensIn + u.TokensOut }

// UsageCell aggregates one bucket for the stats API.
type UsageCell struct {
	Requests    int64 `json:"requests"`
	TokensIn    int64 `json:"tokens_in"`
	TokensOut   int64 `json:"tokens_out"`
	TokensTotal int64 `json:"tokens_total"`
}

func (c *UsageCell) add(u *DailyUsage) {
	c.Requests += u.Requests
	c.TokensIn += u.TokensIn
	c.TokensOut += u.TokensOut
	c.TokensTotal += u.tokensTotal()
}

// Account is one bailucode upstream account with its bound API key.
type Account struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	Password string `json:"password,omitempty"` // encrypted at rest
	ApiKey   string `json:"api_key,omitempty"`  // encrypted at rest

	Status         string `json:"status"`
	Disabled       bool   `json:"disabled,omitempty"`
	DisabledReason string `json:"disabled_reason,omitempty"`

	Quota *QuotaSnapshot `json:"quota,omitempty"`

	// LastCheckin records the last successful check-in per slot
	// (slots are defined in internal/checkin).
	LastCheckin map[string]time.Time `json:"last_checkin,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	LastUsedAt    time.Time `json:"last_used_at,omitempty"`
	LastSuccessAt time.Time `json:"last_success_at,omitempty"`
	LastError     string    `json:"last_error,omitempty"`

	SuccessCount int64 `json:"success_count,omitempty"`
	FailCount    int64 `json:"fail_count,omitempty"`

	// ConsecutiveFails + CooldownUntil implement the lightweight circuit
	// breaker in internal/pool.
	ConsecutiveFails int       `json:"consecutive_fails,omitempty"`
	CooldownUntil    time.Time `json:"cooldown_until,omitempty"`

	Metadata map[string]string `json:"metadata,omitempty"`
	Notes    string            `json:"notes,omitempty"`
}

// HasApiKey reports whether the account has a bound upstream API key.
func (a *Account) HasApiKey() bool {
	return a != nil && strings.TrimSpace(a.ApiKey) != ""
}

// Callable reports whether the account may serve traffic right now.
func (a *Account) Callable(now time.Time) bool {
	if a == nil || a.Disabled {
		return false
	}
	if !a.HasApiKey() {
		return false
	}
	if a.Status == StatusDisabled || a.Status == StatusError {
		return false
	}
	if !a.CooldownUntil.IsZero() && now.Before(a.CooldownUntil) {
		return false
	}
	return true
}

// PublicView returns the account with secrets redacted for admin APIs.
func (a *Account) PublicView() map[string]any {
	if a == nil {
		return nil
	}
	maskedKey := ""
	if a.HasApiKey() {
		k := strings.TrimSpace(a.ApiKey)
		if len(k) > 8 {
			maskedKey = k[:4] + "****" + k[len(k)-4:]
		} else {
			maskedKey = "****"
		}
	}
	return map[string]any{
		"id":                a.ID,
		"email":             a.Email,
		"status":            a.Status,
		"disabled":          a.Disabled,
		"disabled_reason":   a.DisabledReason,
		"has_apikey":        a.HasApiKey(),
		"apikey_masked":     maskedKey,
		"quota":             a.Quota,
		"last_checkin":      a.LastCheckin,
		"created_at":        a.CreatedAt,
		"updated_at":        a.UpdatedAt,
		"last_used_at":      a.LastUsedAt,
		"last_success_at":   a.LastSuccessAt,
		"last_error":        a.LastError,
		"success_count":     a.SuccessCount,
		"fail_count":        a.FailCount,
		"consecutive_fails": a.ConsecutiveFails,
		"cooldown_until":    a.CooldownUntil,
		"notes":             a.Notes,
	}
}

// NewID mints a random account id.
func NewID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "acc_" + hex.EncodeToString(b)
}

// NewKeyID mints a random downstream key id.
func NewKeyID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "key_" + hex.EncodeToString(b)
}

// NewKeySecret mints a fresh downstream key secret (shown to the admin once).
func NewKeySecret() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return "sk-b2a-" + hex.EncodeToString(b)
}

type DB struct {
	mu          sync.Mutex
	path        string
	Accounts    map[string]*Account `json:"accounts"`
	ApiKeys     map[string]*ApiKey  `json:"api_keys"`
	Usage       map[string]*DailyUsage `json:"usage"`
	Version     int                 `json:"version"`
	fieldCipher *FieldCipher
}

func Open(dataDir string) (*DB, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	fieldCipher, err := newFieldCipher(dataDir)
	if err != nil {
		return nil, fmt.Errorf("initialize field cipher: %w", err)
	}
	path := filepath.Join(dataDir, "accounts.json")
	db := &DB{
		path:        path,
		Accounts:    map[string]*Account{},
		ApiKeys:     map[string]*ApiKey{},
		Usage:       map[string]*DailyUsage{},
		fieldCipher: fieldCipher,
	}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, db)
	}
	if db.Accounts == nil {
		db.Accounts = map[string]*Account{}
	}
	if db.ApiKeys == nil {
		db.ApiKeys = map[string]*ApiKey{}
	}
	if db.Usage == nil {
		db.Usage = map[string]*DailyUsage{}
	}
	for _, a := range db.Accounts {
		if a == nil {
			continue
		}
		if err := db.decryptSensitive(a); err != nil {
			return nil, fmt.Errorf("decrypt sensitive fields for %s: %w", a.ID, err)
		}
	}
	for _, k := range db.ApiKeys {
		if k == nil {
			continue
		}
		var err error
		if k.Secret, err = db.fieldCipher.decrypt(k.Secret); err != nil {
			return nil, fmt.Errorf("decrypt api key %s: %w", k.ID, err)
		}
	}
	return db, nil
}

// saveLocked serializes an isolated snapshot with encrypted sensitive fields.
// Callers must hold db.mu.
func (db *DB) saveLocked() error {
	type diskSnapshot struct {
		Accounts map[string]*Account    `json:"accounts"`
		ApiKeys  map[string]*ApiKey     `json:"api_keys"`
		Usage    map[string]*DailyUsage `json:"usage"`
		Version  int                    `json:"version"`
	}
	plain, err := json.Marshal(&diskSnapshot{Accounts: db.Accounts, ApiKeys: db.ApiKeys, Usage: db.Usage, Version: db.Version})
	if err != nil {
		return err
	}
	var snapshot diskSnapshot
	if err := json.Unmarshal(plain, &snapshot); err != nil {
		return err
	}
	for _, a := range snapshot.Accounts {
		db.encryptSensitive(a)
	}
	for _, k := range snapshot.ApiKeys {
		if k != nil && k.Secret != "" {
			k.Secret = db.fieldCipher.encrypt(k.Secret)
		}
	}
	data, err := json.MarshalIndent(&snapshot, "", "  ")
	if err != nil {
		return err
	}
	tmp := db.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil { // 0600 — owner only
		return err
	}
	return os.Rename(tmp, db.path)
}

func (db *DB) isEncrypted(v string) bool { return strings.HasPrefix(v, "enc:") }

func (db *DB) decryptSensitive(a *Account) error {
	if a == nil {
		return nil
	}
	var err error
	if a.Password, err = db.fieldCipher.decrypt(a.Password); err != nil {
		return err
	}
	if a.ApiKey, err = db.fieldCipher.decrypt(a.ApiKey); err != nil {
		return err
	}
	return nil
}

func (db *DB) encryptSensitive(a *Account) {
	if a == nil {
		return
	}
	a.Password = db.fieldCipher.encrypt(a.Password)
	a.ApiKey = db.fieldCipher.encrypt(a.ApiKey)
}

func (db *DB) List() []*Account {
	db.mu.Lock()
	defer db.mu.Unlock()
	out := make([]*Account, 0, len(db.Accounts))
	for _, a := range db.Accounts {
		out = append(out, a)
	}
	return out
}

func (db *DB) Get(id string) (*Account, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	a, ok := db.Accounts[id]
	if !ok {
		return nil, errors.New("account not found")
	}
	return a, nil
}

// FindByEmail returns the account with the given email (case-insensitive).
func (db *DB) FindByEmail(email string) *Account {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return nil
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	for _, a := range db.Accounts {
		if strings.ToLower(strings.TrimSpace(a.Email)) == email {
			return a
		}
	}
	return nil
}

func (db *DB) Upsert(a *Account) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if a.ID == "" {
		a.ID = NewID()
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now()
	}
	a.UpdatedAt = time.Now()
	db.Accounts[a.ID] = a
	return db.saveLocked()
}

func (db *DB) Delete(id string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if _, ok := db.Accounts[id]; !ok {
		return errors.New("account not found")
	}
	delete(db.Accounts, id)
	return db.saveLocked()
}

// Save forces a flush; called on shutdown.
func (db *DB) Save() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.saveLocked()
}

// ---------------------------------------------------------------- keys

// CreateApiKey stores a new downstream key. Empty secret auto-generates one.
func (db *DB) CreateApiKey(name, secret string) (*ApiKey, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("名称不能为空")
	}
	secret = strings.TrimSpace(secret)
	if secret == "" {
		secret = NewKeySecret()
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	for _, k := range db.ApiKeys {
		if k != nil && k.Secret == secret {
			return nil, errors.New("该 Key 已存在")
		}
	}
	key := &ApiKey{
		ID:        NewKeyID(),
		Name:      name,
		Secret:    secret,
		Prefix:    keyPrefix(secret),
		Enabled:   true,
		CreatedAt: time.Now(),
	}
	db.ApiKeys[key.ID] = key
	if err := db.saveLocked(); err != nil {
		return nil, err
	}
	return key, nil
}

// keyPrefix renders a short display hint for a secret.
func keyPrefix(secret string) string {
	if len(secret) > 12 {
		return secret[:9] + "…" + secret[len(secret)-4:]
	}
	return secret
}

// ListApiKeys returns all downstream keys ordered by creation time.
func (db *DB) ListApiKeys() []*ApiKey {
	db.mu.Lock()
	defer db.mu.Unlock()
	out := make([]*ApiKey, 0, len(db.ApiKeys))
	for _, k := range db.ApiKeys {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// DeleteApiKey removes a downstream key.
func (db *DB) DeleteApiKey(id string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if _, ok := db.ApiKeys[id]; !ok {
		return errors.New("key not found")
	}
	delete(db.ApiKeys, id)
	return db.saveLocked()
}

// ToggleApiKey flips a key's enabled flag.
func (db *DB) ToggleApiKey(id string) (*ApiKey, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	k, ok := db.ApiKeys[id]
	if !ok || k == nil {
		return nil, errors.New("key not found")
	}
	k.Enabled = !k.Enabled
	if err := db.saveLocked(); err != nil {
		return nil, err
	}
	return k, nil
}

// MatchApiKey finds the enabled key matching the presented secret.
// Constant-time per candidate; key counts are small.
func (db *DB) MatchApiKey(secret string) *ApiKey {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return nil
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	for _, k := range db.ApiKeys {
		if k == nil || !k.Enabled {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(k.Secret), []byte(secret)) == 1 {
			return k
		}
	}
	return nil
}

// ---------------------------------------------------------------- usage

// RecordUsage upserts the daily rollup for (key, account, model).
func (db *DB) RecordUsage(keyID, accountID, model string, tokensIn, tokensOut int64) {
	date := time.Now().Format("2006-01-02")
	db.mu.Lock()
	defer db.mu.Unlock()
	id := date + "|" + keyID + "|" + accountID + "|" + model
	u, ok := db.Usage[id]
	if !ok {
		u = &DailyUsage{Date: date, KeyID: keyID, AccountID: accountID, Model: model}
		db.Usage[id] = u
	}
	u.Requests++
	u.TokensIn += tokensIn
	u.TokensOut += tokensOut
	_ = db.saveLocked()
}

// UsageStats aggregates usage for the admin stats API.
func (db *DB) UsageStats(days int) map[string]any {
	if days <= 0 {
		days = 14
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	total := &UsageCell{}
	todayStr := time.Now().Format("2006-01-02")
	today := &UsageCell{}
	byDate := map[string]*UsageCell{}
	byKey := map[string]*UsageCell{}
	byAccount := map[string]*UsageCell{}
	for _, u := range db.Usage {
		if u == nil {
			continue
		}
		total.add(u)
		if u.Date == todayStr {
			today.add(u)
		}
		if c, ok := byDate[u.Date]; ok {
			c.add(u)
		} else {
			c := &UsageCell{}
			c.add(u)
			byDate[u.Date] = c
		}
		if u.KeyID != "" {
			if c, ok := byKey[u.KeyID]; ok {
				c.add(u)
			} else {
				c := &UsageCell{}
				c.add(u)
				byKey[u.KeyID] = c
			}
		}
		if u.AccountID != "" {
			if c, ok := byAccount[u.AccountID]; ok {
				c.add(u)
			} else {
				c := &UsageCell{}
				c.add(u)
				byAccount[u.AccountID] = c
			}
		}
	}
	// 最近 N 天序列（含空日）
	daily := make([]map[string]any, 0, days)
	for i := days - 1; i >= 0; i-- {
		d := time.Now().AddDate(0, 0, -i).Format("2006-01-02")
		c := byDate[d]
		if c == nil {
			c = &UsageCell{}
		}
		daily = append(daily, map[string]any{"date": d, "requests": c.Requests,
			"tokens_in": c.TokensIn, "tokens_out": c.TokensOut, "tokens_total": c.TokensTotal})
	}
	return map[string]any{
		"total":      total,
		"today":      today,
		"daily":      daily,
		"by_key":     byKey,
		"by_account": byAccount,
	}
}
