package api

import (
	"net/http"
	"time"
)

// CompactionItem is the public JSON shape per row.
type CompactionItem struct {
	ID                int64    `json:"id"`
	SessionUUID       string   `json:"session_uuid"`
	Project           string   `json:"project"`
	TS                string   `json:"ts"`
	TSUnixMS          int64    `json:"ts_unix_ms"`
	PrefixTokensEst   int      `json:"prefix_tokens_est"`
	SummaryTokensEst  int      `json:"summary_tokens_est"`
	GapToPrevS        *float64 `json:"gap_to_prev_s,omitempty"`
	CacheState        string   `json:"cache_state"`
	Confirmed         bool     `json:"confirmed"`
	ConfirmReason     string   `json:"confirm_reason,omitempty"`
}

// CompactionsResponse rolls up totals + lists.
type CompactionsResponse struct {
	Items          []CompactionItem `json:"items"`
	Count          int              `json:"count"`
	ByCacheState   map[string]int   `json:"by_cache_state"`
	ColdCount      int              `json:"cold_count"`
	WarmCount      int              `json:"warm_count"`
	UnknownCount   int              `json:"unknown_count"`
	WastedTokens   int64            `json:"wasted_tokens"` // sum of prefix re-cache cost on cold compactions
}

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

	out := CompactionsResponse{
		ByCacheState: map[string]int{},
	}
	for rows.Next() {
		var (
			it       CompactionItem
			gap      *float64
			conf     int
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
