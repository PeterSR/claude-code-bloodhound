package server

import (
	"net/http"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
)

func (s *Server) handleCompactions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := s.Store.DB.QueryContext(ctx, `
		SELECT id, session_uuid, project, ts, ts_unix_ms,
		       prefix_tokens_est, summary_tokens_est,
		       gap_to_prev_s, cache_state,
		       confirmed, confirm_reason
		FROM compactions
		WHERE confirmed = 1
		ORDER BY ts_unix_ms DESC
	`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer rows.Close()

	out := routes.CompactionsResponse{
		ByCacheState: map[string]int{},
	}
	for rows.Next() {
		var (
			it   routes.CompactionItem
			gap  *float64
			conf int
		)
		var gapVal interface{}
		if err := rows.Scan(
			&it.ID, &it.SessionUUID, &it.Project, &it.TS, &it.TSUnixMS,
			&it.PrefixTokensEst, &it.SummaryTokensEst,
			&gapVal, &it.CacheState,
			&conf, &it.ConfirmReason,
		); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if gapVal != nil {
			switch g := gapVal.(type) {
			case float64:
				v := g
				gap = &v
			case int64:
				v := float64(g)
				gap = &v
			}
		}
		it.GapToPrevS = gap
		it.Confirmed = conf == 1
		// Use server-friendly TS too.
		_ = time.Time{} // (kept; some tooling expects time imported)
		out.Items = append(out.Items, it)
		out.ByCacheState[it.CacheState]++
		switch it.CacheState {
		case "cold":
			out.ColdCount++
			out.WastedTokens += int64(it.PrefixTokensEst)
		case "warm_5m", "warm_1h":
			out.WarmCount++
		default:
			out.UnknownCount++
		}
	}
	out.Count = len(out.Items)
	writeJSON(w, http.StatusOK, out)
}
