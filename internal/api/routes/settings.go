package routes

import (
	"github.com/PeterSR/claude-code-bloodhound/internal/config"
)

// SettingsResponse exposes the saved config plus the path on disk so the
// UI can show the user where to edit it directly if they prefer.
type SettingsResponse struct {
	Config     config.Config    `json:"config"`
	ConfigPath string           `json:"config_path"`
	StatePath  string           `json:"state_path"`
	DataPath   string           `json:"data_path"`
	Snippets   SettingsSnippets `json:"snippets"`
}

// SettingsSnippets bundles the copy-paste blobs the Settings page surfaces.
// Keeping them server-side means we can adjust the snippet shape without
// shipping a frontend update.
type SettingsSnippets struct {
	Statusline     string `json:"statusline"`
	StopHook       string `json:"stop_hook"`
	UserPromptHook string `json:"user_prompt_hook"`
	SystemdUnit    string `json:"systemd_user_unit"`
	SystemdTimer   string `json:"systemd_user_timer"`
}
