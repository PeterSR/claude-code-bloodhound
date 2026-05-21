package api

import (
	"context"
	"net/http"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/config"
	"github.com/PeterSR/claude-code-bloodhound/internal/usage"
)

// handleExtractorRetrain runs a one-shot self-heal cycle: fetch the current
// /usage panel and ask `claude -p` to generate a fresh extractor JSON, then
// persist it. Available regardless of the extractor_self_heal config flag,
// since this is an explicit user-driven action from the Debug page.
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

	// Generous budget — bootstrap runs `claude -p` which itself can take
	// 30+ seconds on a cold start. Frontend should show a long-running
	// indicator.
	ctx, cancel := context.WithTimeout(r.Context(), 180*time.Second)
	defer cancel()

	res, fetchErr := usage.Fetch(ctx, usage.Options{ClaudeBinary: cfg.ClaudeBinary})
	if fetchErr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok":    false,
			"error": "capture: " + fetchErr.Error(),
		})
		return
	}
	if res.RawFull == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok":    false,
			"error": "captured panel is empty; nothing to learn from",
		})
		return
	}

	info, bErr := usage.Bootstrap(ctx, usage.BootstrapOptions{
		ClaudeBinary: cfg.ClaudeBinary,
		Panel:        res.RawFull,
	})
	if bErr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok":    false,
			"error": "bootstrap: " + bErr.Error(),
		})
		return
	}

	res = usage.Reapply(res, info.Extractor)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"elapsed_s":        info.ElapsedS,
		"fields":           len(info.Extractor.Fields),
		"saved_at":         info.SavedAt,
		"extractor_origin": res.ExtractorOrigin,
		"applied":          res.OK,
		"missing":          res.Extracted.Missing,
	})
}
