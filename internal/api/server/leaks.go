package server

import (
	"net/http"

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
	"github.com/PeterSR/claude-code-bloodhound/internal/sessioninsight"
)

func (s *Server) handleLeaks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := routes.LeaksResponse{}

	// Per-class token sums. We use raw (in + out + cr + cw5m + cw1h) here
	// because the "leak" concept is about volume the user could have
	// avoided. Cost-weighted is in TotalCWTokens for context only.
	classRows, err := s.Store.DB.QueryContext(ctx, `
		SELECT classification,
		       COUNT(*) AS n,
		       COALESCE(SUM(`+sessioninsight.RawExpr+`), 0) AS raw,
		       COALESCE(SUM(`+sessioninsight.CWExpr()+`), 0) AS cw
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
		var row routes.SessionLeakRow
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
