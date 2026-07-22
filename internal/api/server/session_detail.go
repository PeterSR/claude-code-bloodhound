package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
	"github.com/PeterSR/claude-code-bloodhound/internal/attribute"
)

func (s *Server) handleSessionDetail(w http.ResponseWriter, r *http.Request) {
	uuid := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	uuid = strings.TrimSuffix(uuid, "/")
	if uuid == "" || strings.Contains(uuid, "/") {
		http.NotFound(w, r)
		return
	}

	ctx := r.Context()

	// Header from the materialized sessions table.
	var resp routes.SessionDetailResponse
	resp.SessionUUID = uuid
	resp.Turns = []routes.TurnItem{}
	resp.Compactions = []routes.CompactionItem{}
	var firstMS, lastMS int64
	err := s.Store.DB.QueryRowContext(ctx, `
		SELECT project, first_ts_unix_ms, last_ts_unix_ms,
		       turn_count, raw_tokens, output_tokens, peak_5h_raw_tokens,
		       idle_miss_count, rotation_count, restructure_count,
		       compaction_count, cold_compaction_count,
		       cache_ttl, models
		FROM sessions WHERE session_uuid = ?
	`, uuid).Scan(
		&resp.Project, &firstMS, &lastMS,
		&resp.TurnCount, &resp.RawTokens, &resp.OutputTokens, &resp.Peak5hRawTokens,
		&resp.IdleMissCount, &resp.RotationCount, &resp.RestructureCount,
		&resp.CompactionCount, &resp.ColdCompactionCount,
		&resp.CacheTTL, &resp.Models,
	)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	resp.FirstTS = time.UnixMilli(firstMS).UTC().Format(time.RFC3339)
	resp.LastTS = time.UnixMilli(lastMS).UTC().Format(time.RFC3339)

	// Limit attribution. A session with no attribution yet (ingested since
	// the last aggregate pass) still renders; the panel just reads as
	// pending rather than as zero cost.
	totals, err := s.Store.SessionPctTotalsAll(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	resp.Attribution.SessionAttribution = sessionAttribution(totals[uuid])
	if resp.Attribution.Week, err = s.sessionWindowSlices(ctx, uuid, attribute.BucketWeek); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if resp.Attribution.FivH, err = s.sessionWindowSlices(ctx, uuid, attribute.Bucket5h); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	trows, err := s.Store.DB.QueryContext(ctx, `
		SELECT turn_idx, ts, ts_unix_ms, model,
		       input_tokens, output_tokens, cache_read,
		       cache_create_5m, cache_create_1h,
		       gap_s, classification, post_compact
		FROM turns
		WHERE session_uuid = ?
		ORDER BY turn_idx ASC
	`, uuid)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer trows.Close()
	for trows.Next() {
		var (
			t  routes.TurnItem
			pc int
		)
		if err := trows.Scan(
			&t.TurnIdx, &t.TS, &t.TSUnixMS, &t.Model,
			&t.InputTokens, &t.OutputTokens, &t.CacheRead,
			&t.CacheCreate5m, &t.CacheCreate1h,
			&t.GapS, &t.Classification, &pc,
		); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		t.PostCompact = pc == 1
		resp.Turns = append(resp.Turns, t)
	}

	crows, err := s.Store.DB.QueryContext(ctx, `
		SELECT id, session_uuid, project, ts, ts_unix_ms,
		       prefix_tokens_est, summary_tokens_est,
		       gap_to_prev_s, cache_state,
		       confirmed, confirm_reason
		FROM compactions
		WHERE session_uuid = ?
		ORDER BY ts_unix_ms ASC
	`, uuid)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer crows.Close()
	for crows.Next() {
		var (
			it     routes.CompactionItem
			gapVal interface{}
			conf   int
		)
		if err := crows.Scan(
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
				it.GapToPrevS = &v
			case int64:
				v := float64(g)
				it.GapToPrevS = &v
			}
		}
		it.Confirmed = conf == 1
		resp.Compactions = append(resp.Compactions, it)
	}

	writeJSON(w, http.StatusOK, resp)
}
