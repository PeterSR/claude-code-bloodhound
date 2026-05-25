package server

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
	"github.com/PeterSR/claude-code-bloodhound/internal/config"
)

const (
	statuslineSnippet = `# Claude Code statusline. Renders one line of session/week %.
# Add to ~/.config/claude/settings.json under "statusline":
{
  "statusline": "bloodhound status"
}`
	stopHookSnippet = `# Claude Code Stop hook. Re-runs ingest when the assistant
# finishes a turn so the UI is fresh.
# Add to ~/.config/claude/settings.json under "hooks":
{
  "hooks": {
    "Stop": [
      { "command": "bloodhound ingest --quiet" }
    ]
  }
}`
	userPromptHookSnippet = `# Claude Code UserPromptSubmit hook. Lets bloodhound mark when
# you replied so it knows whether the cache window stayed warm.
{
  "hooks": {
    "UserPromptSubmit": [
      { "command": "bloodhound notify-if --reset-idle-timer" }
    ]
  }
}`
	systemdUnitSnippet = `# ~/.config/systemd/user/bloodhound.service
[Unit]
Description=Claude Code Bloodhound — usage collector
After=default.target

[Service]
Type=simple
ExecStart=%s daemon
Restart=on-failure

[Install]
WantedBy=default.target`
	systemdTimerSnippet = `# ~/.config/systemd/user/bloodhound.timer
[Unit]
Description=Run Bloodhound continuously

[Timer]
OnBootSec=30s
OnUnitActiveSec=5min
Persistent=true

[Install]
WantedBy=timers.target`
)

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.getSettings(w, r)
	case http.MethodPut, http.MethodPost:
		s.putSettings(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) getSettings(w http.ResponseWriter, _ *http.Request) {
	cfg, err := config.Load()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	cfgPath, _ := config.Path()
	statePath, _ := config.StateDir()
	dataPath, _ := config.DataDir()
	resp := routes.SettingsResponse{
		Config:     cfg,
		ConfigPath: cfgPath,
		StatePath:  statePath,
		DataPath:   dataPath,
		Snippets: routes.SettingsSnippets{
			Statusline:     statuslineSnippet,
			StopHook:       stopHookSnippet,
			UserPromptHook: userPromptHookSnippet,
			// We don't know the user's binary path here; daemon command
			// is what matters, so leave the placeholder.
			SystemdUnit:  fmt.Sprintf(systemdUnitSnippet, "/path/to/bloodhound"),
			SystemdTimer: systemdTimerSnippet,
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var cfg config.Config
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if err := config.Save(cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
