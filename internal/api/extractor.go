package api

import (
	"bytes"
	"context"
	"net/http"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/config"
	"github.com/PeterSR/claude-code-bloodhound/internal/usage/selfheal"
)

// handleExtractorRetrain triggers one orchestrator-driven self-heal:
// daemon spawns an inner claude in a pty and lets an orchestrator
// claude -p drive it via MCP tools until a fresh extractor is saved.
// Always available regardless of the extractor_self_heal config flag —
// this is an explicit user-driven action from the Debug page.
func (s *Server) handleExtractorRetrain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cfg, err := config.Load()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	// Generous budget — the orchestrator runs claude -p which can take
	// 30-60s on a cold start, plus a fresh inner claude session.
	ctx, cancel := context.WithTimeout(r.Context(), 180*time.Second)
	defer cancel()

	stderr := &bytes.Buffer{}
	heal := selfheal.Run(ctx, selfheal.Options{
		ClaudeBinary: cfg.ClaudeBinary,
		Timeout:      150 * time.Second,
		Stderr:       stderr,
	})

	if !heal.OK {
		errMsg := "self-heal failed"
		if heal.Err != nil {
			errMsg = heal.Err.Error()
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok":          false,
			"error":       errMsg,
			"stderr_tail": heal.StderrTail,
			"total_ms":    heal.TotalMs,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"saved_at":        heal.SavedAt,
		"orchestrator_ms": heal.OrchestratorMs,
		"total_ms":        heal.TotalMs,
		"stderr_tail":     heal.StderrTail,
		"applied":         true,
	})
}
