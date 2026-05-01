package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

const configFileName = "config.json"

// PlanTier values used for community-insights metadata. They have no effect
// on local computation.
const (
	PlanUnknown = "unknown"
	PlanPro     = "pro"
	PlanMax5    = "max-5x"
	PlanMax20   = "max-20x"
)

// Config is the JSON-serialized user configuration.
//
// Fields that aren't present in the file fall back to Default(). Missing
// config file => returns Default() with no error.
type Config struct {
	Host string `json:"host"`
	Port int    `json:"port"`

	PollIntervalS      int `json:"poll_interval_s"`
	IngestIntervalS    int `json:"ingest_interval_s"`
	AggregateIntervalS int `json:"aggregate_interval_s"`

	// PlanTier is cosmetic — used only as community-insights metadata.
	// Has no effect on the local computation. One of PlanUnknown, PlanPro,
	// PlanMax5, PlanMax20.
	PlanTier string `json:"plan_tier"`

	// JoinThePack toggles the (future) opt-in upload of anonymized usage
	// packets to the community-insights server. v1 has no server, so this
	// flag is purely informational.
	JoinThePack bool `json:"join_the_pack"`

	// UserID, if set, links this device to others belonging to the same
	// account so the community-insights server doesn't double-count users
	// who run Claude Code from multiple machines (the 5h / weekly quotas
	// are pooled per Anthropic account, not per device). Blank => this
	// device is treated as its own user. The store separately persists a
	// random device_id; together they form the (user, device) identity.
	UserID string `json:"user_id"`

	// ClaudeBinary overrides the path to the `claude` executable used by the
	// /usage scraper. Empty means look up "claude" on $PATH.
	ClaudeBinary string `json:"claude_binary"`

	// StatuslinePrefix is prepended to the `bloodhound status` output. The
	// default emoji renders nicely in most modern terminals; on terminals
	// that don't, set it to "BH" or "" via config.json.
	StatuslinePrefix string `json:"statusline_prefix"`

	// StaleAfterS is the threshold for "stale" badges in the UI and for
	// the statusline's STALE indicator. Defaults to 600 (10 min) — twice
	// the default poll cadence.
	StaleAfterS int `json:"stale_after_s"`

	// ActiveSessionThresholdS is the cutoff for marking a recent session
	// as "active" on the Now page. Sessions whose last turn is within
	// this many seconds get the Active badge. Multiple sessions can
	// qualify simultaneously — matches the workflow where a user has
	// several Claude Code windows open at once.
	ActiveSessionThresholdS int `json:"active_session_threshold_s"`

	// RecentSessionWindowS is how far back the Now page's recent-sessions
	// panel looks. Sessions whose last turn is older than this are
	// excluded entirely. Defaults to 24h.
	RecentSessionWindowS int `json:"recent_session_window_s"`
}

// Default returns the baseline config. New installs start here.
func Default() Config {
	return Config{
		Host:               "127.0.0.1",
		Port:               7777,
		PollIntervalS:      300,  // 5 min
		IngestIntervalS:    300,  // 5 min
		AggregateIntervalS: 900,  // 15 min
		PlanTier:           PlanUnknown,
		JoinThePack:        false,
		ClaudeBinary:       "",
		StatuslinePrefix:        "🩸",
		StaleAfterS:             600,
		ActiveSessionThresholdS: 1800,
		RecentSessionWindowS:    86400,
	}
}

// Path returns the absolute path to the config file.
func Path() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, configFileName), nil
}

// Load reads the config file and merges it onto Default(). A missing file is
// not an error; the returned config is the default.
func Load() (Config, error) {
	cfg := Default()
	p, err := Path()
	if err != nil {
		return cfg, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read config: %w", err)
	}
	// Unmarshal onto the default-populated struct so that missing keys keep
	// their default values.
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

// Save writes the config back out, creating the directory if needed.
func Save(cfg Config) error {
	dir, err := ConfigDir()
	if err != nil {
		return err
	}
	if err := EnsureDir(dir); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	p := filepath.Join(dir, configFileName)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}
