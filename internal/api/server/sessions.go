package server

import (
	"net/http"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
)

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	acct, ok := s.withAccount(w, r)
	if !ok {
		return
	}
	rows, err := s.Store.ListSessions(ctx, acct)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// One grouped query for the whole list rather than a lookup per row:
	// the table is a few thousand rows at most and the join would otherwise
	// be N+1.
	pct, err := s.Store.SessionPctTotalsAll(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	out := make([]routes.SessionListItem, 0, len(rows))
	for _, sess := range rows {
		out = append(out, routes.SessionListItem{
			Attribution:         sessionAttribution(pct[sess.SessionUUID]),
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
			SubagentCount:       sess.SubagentCount,
			SubagentTurnCount:   sess.SubagentTurnCount,
		})
	}
	writeJSON(w, http.StatusOK, routes.SessionsResponse{
		Sessions: out,
		Count:    len(out),
	})
}
