// Package sessioninsight derives per-session signals (last-turn cost,
// cache state, /compact recommendation, cold-resume cost) from the local
// turns table. It is shared by the Now page (`internal/api/now.go`) and
// the statusline renderer so the two surfaces never drift on
// thresholds, weighting, or wording.
package sessioninsight

import (
	"context"
	"database/sql"
	"time"
)

// CWExpr is the SQL fragment that converts a turn's raw token columns
// into cost-weighted tokens, mirroring the calibrator's weighting
// (input ×1, output ×5, cache_read ×0.1, cache_create_5m ×1.25,
// cache_create_1h ×2).
const CWExpr = `(input_tokens
                 + output_tokens * 5.0
                 + cache_read * 0.1
                 + cache_create_5m * 1.25
                 + cache_create_1h * 2.0)`

// RawExpr is the SQL fragment summing every per-turn token category
// without weighting — used for the headline "raw" totals.
const RawExpr = `(input_tokens + output_tokens + cache_read + cache_create_5m + cache_create_1h)`

// ColdPrefixCWExpr is the cost-weighted price of replaying the
// conversation prefix when the cache has gone cold. Bounds the lower
// estimate of "what would resuming this session cost" — we can't
// predict the new prompt or response size, but the prefix itself is
// fixed and must be re-paid as cache creation. The prefix at turn N is
// approximated by what the model saw as input + what it produced as
// output during that turn.
const ColdPrefixCWExpr = `((input_tokens + cache_read + cache_create_5m + cache_create_1h + output_tokens) * 1.25)`

// SessionRef identifies one session for downstream insight queries.
// The values come straight from the `sessions` table and are enough
// to drive ForSession without re-querying.
type SessionRef struct {
	UUID     string
	Project  string
	CacheTTL string // session aggregate: "5m" | "1h" | ""
	LastMS   int64
}

// Insight is the per-session summary surfaced on the Now page and
// (in compact form) the bloodhound statusline. Field names and JSON
// tags are stable — they are part of the /api/now wire contract.
type Insight struct {
	SessionUUID  string `json:"session_uuid"`
	Project      string `json:"project"`
	LastTSISO    string `json:"last_ts"`
	LastTSUnixMS int64  `json:"last_ts_unix_ms"`
	AgeS         int64  `json:"age_s"`

	TurnCount      int     `json:"turn_count"`
	TotalRawTokens int64   `json:"total_raw_tokens"`
	TotalCWTokens  float64 `json:"total_cw_tokens"`

	// Last turn's actual cost.
	LastTurnRawTokens int64   `json:"last_turn_raw_tokens"`
	LastTurnCWTokens  float64 `json:"last_turn_cw_tokens"`
	LastTurnPct       float64 `json:"last_turn_pct,omitempty"` // estimated using TokensPerPctCW

	// Recent3AvgCWTokens averages the last min(3, turn_count) turns. The
	// UI compares this to SessionAvgCWTokens to flag context bloat.
	Recent3AvgCWTokens float64 `json:"recent3_avg_cw_tokens,omitempty"`
	Recent3AvgPct      float64 `json:"recent3_avg_pct,omitempty"`
	SessionAvgCWTokens float64 `json:"session_avg_cw_tokens,omitempty"`
	SessionAvgPct      float64 `json:"session_avg_pct,omitempty"`

	// TurnsSinceCompact = turns since the most recent post_compact=1
	// turn, or TurnCount if this session has never been compacted.
	TurnsSinceCompact int `json:"turns_since_compact,omitempty"`

	// CompactCostCWTokens / CompactCostPct estimate what running
	// /compact right now would cost. /compact mechanically resembles
	// one normal turn (current context as input, summary as output),
	// so we use the smoothed recent-turn cost as a first-order
	// estimate. Only set when the recommendation suggests compacting
	// could be worthwhile — the field is the answer to "how much does
	// it cost to act on this?"
	CompactCostCWTokens float64 `json:"compact_cost_cw_tokens,omitempty"`
	CompactCostPct      float64 `json:"compact_cost_pct,omitempty"`

	// ColdResumeCostCWTokens / ColdResumeCostPct estimate the cost of
	// replaying the conversation prefix when the cache has gone cold —
	// the floor on what continuing this session will cost. We can't
	// know the next prompt or response size, but the prefix itself is
	// fixed; everything in it has to be re-cached at the cache_create
	// rate. Computed off the most recent turn's view of the context.
	// Only set when the session's age has actually crossed its cache
	// TTL (5m default, 1h for 1h-TTL sessions); within the TTL the
	// cache is still warm and showing this cost would be misleading.
	ColdResumeCostCWTokens float64 `json:"cold_resume_cost_cw_tokens,omitempty"`
	ColdResumeCostPct      float64 `json:"cold_resume_cost_pct,omitempty"`

	// TokensPerPctCW echoes the latest median used for the % conversions
	// above. Lets the UI render "(at ≈X tok/1%)" without a second call.
	TokensPerPctCW float64 `json:"tokens_per_pct_cw,omitempty"`

	// Recommendation is "" | "ok" | "watch" | "compact". Reason
	// explains why for the UI tooltip.
	Recommendation       string `json:"recommendation,omitempty"`
	RecommendationReason string `json:"recommendation_reason,omitempty"`

	// LastUserPrompt is a short preview (≤240 chars) of the most
	// recent human-typed prompt in this session. Disambiguates cards
	// that share a project name; also doubles as a "where did I leave
	// off" hint. Populated by callers via LastUserPrompt; ForSession
	// itself does not touch this field.
	LastUserPrompt string `json:"last_user_prompt,omitempty"`

	// CacheTTLS is the detected TTL (seconds) of the cache the most
	// recent turn wrote — 300 for 5m, 3600 for 1h, or 0 when we
	// couldn't infer it from per-turn cache_create columns or the
	// session aggregate.
	CacheTTLS int64 `json:"cache_ttl_s,omitempty"`

	// CacheExpiresInS is positive when the cache is still warm but
	// within the last 20% of its TTL window — i.e. about to expire.
	// Populated only in that warning band; otherwise omitted
	// (already-expired sessions surface ColdResumeCost instead,
	// fully-warm sessions need no nudging).
	CacheExpiresInS int64 `json:"cache_expires_in_s,omitempty"`
}

// MostRecentActive returns the single most-recent session whose last
// turn is within thresholdS of now, or nil when no session qualifies.
// "Active" here matches the Now page's Active badge — the same cutoff
// used for cfg.ActiveSessionThresholdS.
func MostRecentActive(ctx context.Context, db *sql.DB, now time.Time, thresholdS int) (*SessionRef, error) {
	if thresholdS <= 0 {
		return nil, nil
	}
	cutoffMS := now.UnixMilli() - int64(thresholdS)*1000
	var ref SessionRef
	err := db.QueryRowContext(ctx, `
		SELECT session_uuid, project, last_ts_unix_ms, COALESCE(cache_ttl, '')
		FROM sessions
		WHERE last_ts_unix_ms >= ?
		ORDER BY last_ts_unix_ms DESC LIMIT 1
	`, cutoffMS).Scan(&ref.UUID, &ref.Project, &ref.LastMS, &ref.CacheTTL)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &ref, nil
}

// RecentN returns up to max sessions whose last turn is within windowS
// of now, ordered most-recent first.
func RecentN(ctx context.Context, db *sql.DB, now time.Time, windowS, max int) ([]SessionRef, error) {
	if max <= 0 {
		return nil, nil
	}
	cutoffMS := now.UnixMilli() - int64(windowS)*1000
	rows, err := db.QueryContext(ctx, `
		SELECT session_uuid, project, last_ts_unix_ms, COALESCE(cache_ttl, '')
		FROM sessions
		WHERE last_ts_unix_ms >= ?
		ORDER BY last_ts_unix_ms DESC LIMIT ?
	`, cutoffMS, max)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionRef
	for rows.Next() {
		var ref SessionRef
		if err := rows.Scan(&ref.UUID, &ref.Project, &ref.LastMS, &ref.CacheTTL); err != nil {
			return out, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// BySessionUUID returns the session ref for the given UUID, or nil if
// the session is not in the local store yet (the bloodhound ingester
// runs on a cadence, so a fresh Claude Code session may not be
// recorded at the moment the statusline renders).
func BySessionUUID(ctx context.Context, db *sql.DB, uuid string) (*SessionRef, error) {
	if uuid == "" {
		return nil, nil
	}
	var ref SessionRef
	err := db.QueryRowContext(ctx, `
		SELECT session_uuid, project, last_ts_unix_ms, COALESCE(cache_ttl, '')
		FROM sessions
		WHERE session_uuid = ?
	`, uuid).Scan(&ref.UUID, &ref.Project, &ref.LastMS, &ref.CacheTTL)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &ref, nil
}

// ForSession populates a fresh Insight from per-turn data for one
// session. Returns nil only on a hard query failure — partial data
// still produces a card. LastUserPrompt is intentionally not filled
// here; callers that need it call LastUserPrompt separately to keep
// the per-session SQL count predictable.
func ForSession(ctx context.Context, db *sql.DB, ref SessionRef, now time.Time, tokensPerPctCW float64, hasCalibration bool) *Insight {
	info := &Insight{
		SessionUUID:  ref.UUID,
		Project:      ref.Project,
		LastTSISO:    time.UnixMilli(ref.LastMS).UTC().Format(time.RFC3339),
		LastTSUnixMS: ref.LastMS,
		AgeS:         (now.UnixMilli() - ref.LastMS) / 1000,
	}

	var rawSum sql.NullInt64
	var cwSum sql.NullFloat64
	var turnCount sql.NullInt64
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(`+RawExpr+`), 0), COALESCE(SUM(`+CWExpr+`), 0)
		FROM turns WHERE session_uuid = ?
	`, ref.UUID).Scan(&turnCount, &rawSum, &cwSum); err != nil {
		return info
	}
	info.TurnCount = int(turnCount.Int64)
	info.TotalRawTokens = rawSum.Int64
	info.TotalCWTokens = round2(cwSum.Float64)
	if info.TurnCount > 0 {
		info.SessionAvgCWTokens = round2(cwSum.Float64 / float64(info.TurnCount))
	}

	if rows, err := db.QueryContext(ctx, `
		SELECT `+RawExpr+`, `+CWExpr+`, `+ColdPrefixCWExpr+`, cache_create_5m, cache_create_1h
		FROM turns WHERE session_uuid = ?
		ORDER BY turn_idx DESC LIMIT 3
	`, ref.UUID); err == nil {
		defer rows.Close()
		var lastRaw, lastCC5m, lastCC1h int64
		var lastCW, lastColdPrefix, sumCW float64
		var n int
		for rows.Next() {
			var r, cc5m, cc1h int64
			var c, cp float64
			if err := rows.Scan(&r, &c, &cp, &cc5m, &cc1h); err != nil {
				break
			}
			if n == 0 {
				lastRaw = r
				lastCW = c
				lastColdPrefix = cp
				lastCC5m = cc5m
				lastCC1h = cc1h
			}
			sumCW += c
			n++
		}
		info.LastTurnRawTokens = lastRaw
		info.LastTurnCWTokens = round2(lastCW)
		// Cache TTL of the *last turn's* cache — not the session-level
		// cache_ttl summary. A 'mix' session you're actively in cached
		// the most-recent prefix at whatever the last turn used
		// (typically 1h, the Claude Code default). Decide per-turn:
		// 1h if the last turn cached at 1h, else 5m if it cached at
		// 5m, else fall back to the session aggregate. ttlS == 0
		// means we couldn't infer it (last turn was cache_read-only
		// and the session has no cache_create history) — neither
		// cold-resume nor expiry warning applies.
		var ttlS int64 = 0
		switch {
		case lastCC1h > 0:
			ttlS = 3600
		case lastCC5m > 0:
			ttlS = 300
		case ref.CacheTTL == "1h":
			ttlS = 3600
		case ref.CacheTTL == "5m":
			ttlS = 300
		}
		if ttlS > 0 {
			info.CacheTTLS = ttlS
			if info.AgeS >= ttlS {
				info.ColdResumeCostCWTokens = round2(lastColdPrefix)
			} else if remain := ttlS - info.AgeS; remain <= ttlS/5 {
				info.CacheExpiresInS = remain
			}
		}
		if n > 0 {
			info.Recent3AvgCWTokens = round2(sumCW / float64(n))
		}
	}

	var sinceCompact sql.NullInt64
	if err := db.QueryRowContext(ctx, `
		WITH last_compact AS (
		    SELECT COALESCE(MAX(turn_idx), -1) AS idx
		      FROM turns
		     WHERE session_uuid = ? AND post_compact = 1
		)
		SELECT COUNT(*) FROM turns
		 WHERE session_uuid = ?
		   AND turn_idx > (SELECT idx FROM last_compact)
	`, ref.UUID, ref.UUID).Scan(&sinceCompact); err == nil {
		info.TurnsSinceCompact = int(sinceCompact.Int64)
	}

	if hasCalibration && tokensPerPctCW > 0 {
		info.TokensPerPctCW = round2(tokensPerPctCW)
		info.LastTurnPct = round2(info.LastTurnCWTokens / tokensPerPctCW)
		info.SessionAvgPct = round2(info.SessionAvgCWTokens / tokensPerPctCW)
		info.Recent3AvgPct = round2(info.Recent3AvgCWTokens / tokensPerPctCW)
		if info.ColdResumeCostCWTokens > 0 {
			info.ColdResumeCostPct = round2(info.ColdResumeCostCWTokens / tokensPerPctCW)
		}
	}

	if info.SessionAvgCWTokens > 0 && info.TurnCount >= 5 && info.Recent3AvgCWTokens > 0 {
		ratio := info.Recent3AvgCWTokens / info.SessionAvgCWTokens
		switch {
		case ratio >= 2.0:
			info.Recommendation = "compact"
			info.RecommendationReason = "recent turns are >2× the session average — context has likely bloated"
		case ratio >= 1.5:
			info.Recommendation = "watch"
			info.RecommendationReason = "recent turns are running heavier than the session average"
		default:
			info.Recommendation = "ok"
		}
	}

	if info.Recent3AvgCWTokens > 0 {
		info.CompactCostCWTokens = info.Recent3AvgCWTokens
		info.CompactCostPct = info.Recent3AvgPct
	}
	return info
}

// LastUserPrompt returns the most recent user-prompt preview captured
// for the session, or "" when none was captured / on error.
func LastUserPrompt(ctx context.Context, db *sql.DB, uuid string) string {
	if uuid == "" {
		return ""
	}
	var preview sql.NullString
	if err := db.QueryRowContext(ctx, `
		SELECT text_preview FROM user_prompts
		WHERE session_uuid = ?
		ORDER BY ts_unix_ms DESC LIMIT 1
	`, uuid).Scan(&preview); err != nil {
		return ""
	}
	return preview.String
}

func round2(f float64) float64 {
	return float64(int(f*100+0.5)) / 100
}
