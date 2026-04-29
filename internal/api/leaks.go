package api

import (
	"net/http"
)

// LeaksResponse aggregates avoidable spend across all sessions.
//
// "Avoidable" here is shorthand for: the user could plausibly change a
// behavior or workflow setting and pay less. We don't claim these are
// 100% wasted — sometimes you really do need to rotate breakpoints — but
// they're the ones worth surfacing first.
type LeaksResponse struct {
	IdleMissTokens     int64 `json:"idle_miss_tokens"`
	IdleMissTurnCount  int   `json:"idle_miss_turn_count"`
	RotationTokens     int64 `json:"rotation_tokens"`
	RotationTurnCount  int   `json:"rotation_turn_count"`
	RestructureTokens  int64 `json:"restructure_tokens"`
	RestructureCount   int   `json:"restructure_turn_count"`
	ColdCompactionTokens int64 `json:"cold_compaction_tokens"`
	ColdCompactionCount  int   `json:"cold_compaction_count"`

	TotalRawTokens     int64 `json:"total_raw_tokens"`
	TotalCWTokens      float64 `json:"total_cw_tokens"`

	TopOffenders []SessionLeakRow `json:"top_offenders"`
}

// SessionLeakRow is the per-session ranking on the Leaks page.
type SessionLeakRow struct {
	SessionUUID         string  `json:"session_uuid"`
	Project             string  `json:"project"`
	IdleMissCount       int     `json:"idle_miss_count"`
	RotationCount       int     `json:"rotation_count"`
	RestructureCount    int     `json:"restructure_count"`
	ColdCompactionCount int     `json:"cold_compaction_count"`
	AvoidableScore      int64   `json:"avoidable_score"` // sum of leak tokens for this session
	RawTokens           int64   `json:"raw_tokens"`
}

func (s *Server) handleLeaks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := LeaksResponse{}

	// Per-class token sums. We use raw (in + out + cr + cw5m + cw1h) here
	// because the "leak" concept is about volume the user could have
	// avoided. Cost-weighted is in TotalCWTokens for context only.
	classRows, err := s.Store.DB.QueryContext(ctx, `
		SELECT classification,
		       COUNT(*) AS n,
		       COALESCE(SUM(input_tokens + output_tokens + cache_read +
		                    cache_create_5m + cache_create_1h), 0) AS raw,
		       COALESCE(SUM(cache_read*0.1 + cache_create_5m*1.25 +
		                    cache_create_1h*2.0 + input_tokens*1.0 +
		                    output_tokens*5.0), 0) AS cw
		FROM turns
		GROUP BY classification
	`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer classRows.Close()
	for classRows.Next() {
		var class string
		var n int
		var raw int64
		var cw float64
		if err := classRows.Scan(&class, &n, &raw, &cw); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		out.TotalRawTokens += raw
		out.TotalCWTokens += cw
		switch class {
		case "idle_miss":
			out.IdleMissTokens = raw
			out.IdleMissTurnCount = n
		case "rotation":
			out.RotationTokens = raw
			out.RotationTurnCount = n
		case "restructure":
			out.RestructureTokens = raw
			out.RestructureCount = n
		}
	}

	// Cold compaction prefix tokens — the prefix had to be re-cached
	// because cache had expired before the user compacted. Avoidable by
	// compacting earlier (while warm) or by adjusting cache TTL.
	if err := s.Store.DB.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(prefix_tokens_est), 0)
		FROM compactions
		WHERE confirmed = 1 AND cache_state = 'cold'
	`).Scan(&out.ColdCompactionCount, &out.ColdCompactionTokens); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	// Top offenders: sessions ranked by sum of avoidable activity. We use
	// idle_miss + rotation token counts from the per-session sum. The
	// `sessions` table has counts but not per-class tokens; we derive
	// avoidable_score by joining with `turns` directly.
	scoreRows, err := s.Store.DB.QueryContext(ctx, `
		SELECT s.session_uuid, s.project, s.idle_miss_count, s.rotation_count,
		       s.restructure_count, s.cold_compaction_count, s.raw_tokens,
		       COALESCE((
		          SELECT SUM(input_tokens + output_tokens + cache_read +
		                     cache_create_5m + cache_create_1h)
		          FROM turns
		          WHERE turns.session_uuid = s.session_uuid
		            AND classification IN ('idle_miss', 'rotation', 'restructure')
		       ), 0) AS leak_tokens
		FROM sessions s
		ORDER BY leak_tokens DESC
		LIMIT 25
	`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer scoreRows.Close()
	for scoreRows.Next() {
		var row SessionLeakRow
		if err := scoreRows.Scan(
			&row.SessionUUID, &row.Project,
			&row.IdleMissCount, &row.RotationCount,
			&row.RestructureCount, &row.ColdCompactionCount,
			&row.RawTokens, &row.AvoidableScore,
		); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if row.AvoidableScore > 0 || row.ColdCompactionCount > 0 {
			out.TopOffenders = append(out.TopOffenders, row)
		}
	}

	writeJSON(w, http.StatusOK, out)
}
