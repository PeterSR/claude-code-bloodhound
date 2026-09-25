// Package usage drives Claude Code's /usage TUI panel in a pty, renders
// the captured output through a virtual terminal grid, and applies a
// configurable Extractor to pull out the session and week percentages
// plus reset hints.
//
// The extractor is data-driven (regex DSL persisted to $XDG_STATE_HOME) so
// we can tolerate Anthropic redesigning the panel: extraction failures
// trigger the daemon's self-heal (internal/usage/selfheal), which hands
// the live pty to an orchestrator claude -p over MCP tools and lets it
// re-learn the field positions from a fresh capture.
package usage

import (
	"context"
	"regexp"
	"strings"
	"time"
)

// Result is everything we know after scraping. The two-pct accessors
// (SessionPct, WeekPct) are optional because the extractor may not have
// matched them; OK reports whether all required extractor fields were
// present.
type Result struct {
	OK              bool      `json:"ok"`
	FetchedAt       time.Time `json:"fetched_at"`
	ElapsedS        float64   `json:"elapsed_s"`
	Raw             string    `json:"raw"`              // tail of the cleaned terminal output (debug)
	RawFull         string    `json:"-"`                // full cleaned output (kept in-memory for bootstrap; not serialised)
	ExtractorOrigin string    `json:"extractor_origin"` // "user" | "default"

	SessionPct      *int   `json:"session_pct,omitempty"`
	WeekPct         *int   `json:"week_pct,omitempty"`
	SessionResetRaw string `json:"session_reset_raw,omitempty"`
	WeekResetRaw    string `json:"week_reset_raw,omitempty"`
	SessionResetTZ  string `json:"session_reset_tz,omitempty"`
	WeekResetTZ     string `json:"week_reset_tz,omitempty"`

	// Extracted is the raw output of the Extractor, exposed for the
	// Debug page so the user can see exactly what fired and what didn't.
	Extracted Extracted `json:"extracted"`
}

// Options configures Fetch.
type Options struct {
	ClaudeBinary string        // empty => "claude" on PATH
	Timeout      time.Duration // 0 => 22s
	// ConfigDir is the Claude Code config dir whose account to scrape.
	// Empty or the default dir => CLAUDE_CONFIG_DIR unset.
	ConfigDir string
}

// Fetch spawns Claude Code, captures the /usage panel, and applies the
// active Extractor. Returns a populated Result alongside any error so
// callers can persist a failed attempt for debugging.
func Fetch(ctx context.Context, opts Options) (Result, error) {
	if opts.ClaudeBinary == "" {
		opts.ClaudeBinary = "claude"
	}
	if opts.Timeout == 0 {
		opts.Timeout = 22 * time.Second
	}

	t0 := time.Now()
	rawBytes, driveErr := drive(ctx, opts)
	elapsed := time.Since(t0).Seconds()
	// Render the raw pty stream through a virtual terminal grid. This
	// preserves visual spacing (claude positions chars via ANSI cursor
	// moves rather than literal spaces) and drops stale text that was
	// overdrawn during the capture.
	cleaned := renderVT(rawBytes)

	res := Result{
		FetchedAt: time.Now().UTC(),
		ElapsedS:  round2(elapsed),
		Raw:       tail(cleaned, 3000),
		RawFull:   cleaned,
	}
	if driveErr != nil {
		return res, driveErr
	}

	ext, origin, err := LoadExtractor()
	if err != nil {
		return res, err
	}
	res.ExtractorOrigin = string(origin)
	res.Extracted = ext.Apply(cleaned)
	res.OK = len(res.Extracted.Missing) == 0

	if v, ok := res.Extracted.Values["session_pct"].(int); ok {
		res.SessionPct = &v
	}
	if v, ok := res.Extracted.Values["week_pct"].(int); ok {
		res.WeekPct = &v
	}
	if s, ok := res.Extracted.Values["session_reset"].(string); ok {
		res.SessionResetRaw = s
	}
	if s, ok := res.Extracted.Values["week_reset"].(string); ok {
		res.WeekResetRaw = s
	}
	if s, ok := res.Extracted.Values["session_reset_tz"].(string); ok {
		res.SessionResetTZ = s
	}
	if s, ok := res.Extracted.Values["week_reset_tz"].(string); ok {
		res.WeekResetTZ = s
	}
	return res, nil
}

var ansiRe = regexp.MustCompile(strings.Join([]string{
	`\x1b\[[0-?]*[ -/]*[@-~]`, // CSI sequences
	`\x1b\][^\x07]*\x07`,      // OSC ending in BEL
	`\x1b[PX^_].*?\x1b\\`,     // DCS/SOS/PM/APC ending in ST
	`\x1b[()][AB012]`,         // charset designation
	`\x1b[=>]`,                // app keypad mode
	`\x1b[78]`,                // save / restore cursor (ESC 7, ESC 8)
	`\x1bM`,                   // reverse index
	`\x1b\[\?[0-9;]*[a-zA-Z]`, // private mode
}, "|"))

func stripANSI(b []byte) string {
	return ansiRe.ReplaceAllString(string(b), "")
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func round2(f float64) float64 {
	return float64(int(f*100+0.5)) / 100
}
