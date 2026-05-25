package server

import (
	"net/http"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
)

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.ListSessions(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	out := make([]routes.SessionListItem, 0, len(rows))
	for _, sess := range rows {
		out = append(out, routes.SessionListItem{
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
	writeJSON(w, http.StatusOK, routes.SessionsResponse{
		Sessions: out,
		Count:    len(out),
	})
}
