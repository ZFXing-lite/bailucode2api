// quota.go converts a live control-panel Stats into the persisted
// store.QuotaSnapshot, so all quota bookkeeping lives in one place.
package bailu

import (
	"encoding/json"
	"time"

	"bailucode2api/internal/store"
)

// SnapshotFromStats builds the persisted quota snapshot from live stats.
// Snapshot fields semantics:
//   Tokens:  tokens_quota_effective / tokens_used_effective（含定向包生效值）
//   Credits: credits_quota / credits_used（美元额度）
//   Packs:   directed_packs（定向额度包，如 Anthropic 接入定向）
func SnapshotFromStats(st *Stats) *store.QuotaSnapshot {
	if st == nil {
		return nil
	}
	raw, _ := json.Marshal(st)
	packs := make([]store.PackQuota, 0, len(st.DirectedPacks))
	for _, p := range st.DirectedPacks {
		packs = append(packs, store.PackQuota{
			ID: p.ID, Label: p.Label, Used: p.Used, Quota: p.Quota, Pct: p.Pct,
		})
	}
	return &store.QuotaSnapshot{
		UsedTokens:   st.TokensUsed,
		TotalTokens:  st.TokensQuota,
		RemainTokens: st.TokensQuota - st.TokensUsed,
		CreditsUsed:  st.CreditsUsed,
		CreditsQuota: st.CreditsQuota,
		Packs:        packs,
		Raw:          string(raw),
		FetchedAt:    time.Now(),
	}
}
