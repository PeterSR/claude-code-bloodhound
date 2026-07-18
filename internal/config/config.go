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

	// ExtractorSelfHeal toggles the daemon's automatic self-heal: when
	// a poll's required fields go missing, the daemon spawns a fresh
	// orchestrator over a live pty (via MCP tools) and lets it re-learn
	// a working extractor. With it off, the extractor stays as-is and
	// the user can either edit extractors.json by hand or hit the
	// Retrain button on the Debug page. Default true.
	ExtractorSelfHeal bool `json:"extractor_self_heal"`

	// SelfHealMode picks how the heal's orchestrator LLM is invoked.
	//   "interactive" (default) — second interactive `claude` session
	//      driven from Go over a pty. Tokens count against the user's
	//      interactive subscription limits.
	//   "headless" — `claude -p` with --output-format=json. Cleaner
	//      cost/error envelope, but after 2026-06-15 draws from the
	//      Agent SDK $100 credit (then extra usage) rather than the
	//      interactive subscription.
	SelfHealMode string `json:"self_heal_mode"`

	// PriceSelfHeal toggles automatic price discovery: when the daemon
	// sees a model in the logs that the price table doesn't price, it asks
	// a headless `claude` to look up the model's list price on the web and
	// writes it back marked unverified. Unlike the extractor self-heal
	// (which MUST work or the app goes blind), this is best-effort — an
	// un-found price just leaves the model dropped from the cost-weighted
	// analysis, flagged, and the user can hit "find out for me" on the
	// Models page. Default true. Headless tokens draw from the Agent SDK
	// credit pool; disable this if you'd rather price new models by hand.
	PriceSelfHeal bool `json:"price_self_heal"`

	// TrailEnabled turns on the Trail job: a passive watcher that reads
	// recently-active sessions and keeps per-session work briefs + open
	// loops so you can see what's in flight across worktrees. Off by
	// default — it consumes usage (it drives `claude` to summarise).
	TrailEnabled bool `json:"trail_enabled"`

	// TrailMode picks how the Trail analyzer LLM is invoked — same
	// interactive/headless split as SelfHealMode, and the same
	// 2026-06-15 caveat for headless. Defaults to interactive.
	TrailMode string `json:"trail_mode"`

	// TrailIntervalS is the cadence of the Trail job. Defaults to 900
	// (15 min) — slower than ingest; the briefs are an overview, not a
	// live feed.
	TrailIntervalS int `json:"trail_interval_s"`

	// TrailWindowS bounds which sessions Trail considers "active" enough
	// to analyse. Sessions whose last turn is older than this are
	// skipped. Defaults to ActiveSessionThresholdS when zero.
	TrailWindowS int `json:"trail_window_s"`
}

// SelfHealMode values, kept here so callers don't hard-code strings.
const (
	SelfHealModeInteractive = "interactive"
	SelfHealModeHeadless    = "headless"
)

// TrailMode values. Same semantics as the SelfHeal modes; kept separate
// so the two features can diverge (and so the June-15 headless removal
// can be done independently per feature if needed).
const (
	TrailModeInteractive = "interactive"
	TrailModeHeadless    = "headless"
)

// Default returns the baseline config. New installs start here.
func Default() Config {
	return Config{
		PollIntervalS:           300, // 5 min
		IngestIntervalS:         300, // 5 min
		AggregateIntervalS:      900, // 15 min
		PlanTier:                PlanUnknown,
		JoinThePack:             false,
		ClaudeBinary:            "",
		StatuslinePrefix:        "🩸",
		StaleAfterS:             600,
		ActiveSessionThresholdS: 1800,
		RecentSessionWindowS:    86400,
		ExtractorSelfHeal:       true,
		SelfHealMode:            SelfHealModeInteractive,
		PriceSelfHeal:           true,
		TrailEnabled:            false,
		TrailMode:               TrailModeInteractive,
		TrailIntervalS:          900, // 15 min
		TrailWindowS:            0,   // 0 => fall back to ActiveSessionThresholdS
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
