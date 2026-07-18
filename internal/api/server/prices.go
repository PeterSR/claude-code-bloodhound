package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/config"
	"github.com/PeterSR/claude-code-bloodhound/internal/costweight/priceheal"
)

// handlePricesHeal is the "find out for me" button: look up one model's
// price on the web and, if found, write it to the table marked unverified.
// Force is set — the user asked explicitly, so it bypasses the per-model
// cool-down a failed automatic attempt would have set.
func (s *Server) handlePricesHeal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "missing model"})
		return
	}

	cfg, err := config.Load()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 150*time.Second)
	defer cancel()

	res, gateErr := priceheal.Shared().Heal(ctx, body.Model, priceheal.Options{
		ClaudeBinary: cfg.ClaudeBinary,
		Timeout:      120 * time.Second,
		Force:        true,
	})
	if gateErr != nil {
		// The gate refused (another heal is running). Not a failure of the
		// lookup itself — tell the client to try again shortly.
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok": false, "error": res.Reason, "model": body.Model,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          res.Saved,
		"model":       res.Model,
		"found":       res.Found,
		"saved":       res.Saved,
		"price":       res.Price,
		"reason":      res.Reason,
		"cost_usd":    res.CostUSD,
		"duration_ms": res.DurationMs,
	})
}
