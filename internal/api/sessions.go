package api

import (
	"net/http"
	"time"
)

// SessionListItem is the public JSON shape for the Sessions list page.
type SessionListItem struct {
	SessionUUID         string `json:"session_uuid"`
	Project             string `json:"project"`
	FirstTS             string `json:"first_ts"`
	LastTS              string `json:"last_ts"`
	TurnCount           int    `json:"turn_count"`
	RawTokens           int64  `json:"raw_tokens"`
	OutputTokens        int64  `json:"output_tokens"`
	Peak5hRawTokens     int64  `json:"peak_5h_raw_tokens"`
	IdleMissCount       int    `json:"idle_miss_count"`
	RotationCount       int    `json:"rotation_count"`
	RestructureCount    int    `json:"restructure_count"`
	CompactionCount     int    `json:"compaction_count"`
	ColdCompactionCount int    `json:"cold_compaction_count"`
	CacheTTL            string `json:"cache_ttl"`
	Models              string `json:"models"`
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.ListSessions(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	out := make([]SessionListItem, 0, len(rows))
	for _, sess := range rows {
		out = append(out, SessionListItem{
			SessionUUID:         sess.SessionUUID,
			Project:             sess.Project,
			FirstTS:             time.UnixMilli(sess.FirstTSUnixMS).UTC().Format(time.RFC3339),
			LastTS:              time.UnixMilli(sess.LastTSUnixMS).UTC().Format(time.RFC3339),
			TurnCount:           sess.TurnCount,
			RawTokens:           sess.RawTokens,
			OutputTokens:        sess.OutputTokens,
			Peak5hRawTokens:     sess.Peak5hRawTokens,
			IdleMissCount:       sess.IdleMissCount,
			RotationCount:       sess.RotationCount,
			RestructureCount:    sess.RestructureCount,
			CompactionCount:     sess.CompactionCount,
			ColdCompactionCount: sess.ColdCompactionCount,
			CacheTTL:            sess.CacheTTL,
			Models:              sess.Models,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sessions": out,
		"count":    len(out),
	})
}
