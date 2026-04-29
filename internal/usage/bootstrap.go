package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// BootstrapOptions configures Bootstrap.
type BootstrapOptions struct {
	ClaudeBinary string        // empty => "claude"
	Timeout      time.Duration // 0 => 60s
	Panel        string        // the cleaned terminal text used as input
}

// BootstrapInfo summarises a successful bootstrap.
type BootstrapInfo struct {
	Extractor *Extractor
	SavedAt   string  // path the extractor was persisted to
	ElapsedS  float64 // wall time of the claude invocation + validation
}

// Bootstrap regenerates the /usage extractor by asking the local Claude Code
// (`claude -p`) to study a fresh panel snapshot and emit JSON in our DSL.
//
// Defensive throughout: strict JSON parse (allowing for ```json fences that
// might leak through despite the system prompt), schema validation, regex
// compilation, then a *dry-run* against the same panel that bootstrapped it
// — required fields must extract or the new extractor is rejected.
//
// On success, the extractor is persisted to $XDG_STATE_HOME/bloodhound/
// alongside the panel snapshot used to generate it.
func Bootstrap(ctx context.Context, opts BootstrapOptions) (*BootstrapInfo, error) {
	if opts.ClaudeBinary == "" {
		opts.ClaudeBinary = "claude"
	}
	if opts.Timeout == 0 {
		opts.Timeout = 60 * time.Second
	}
	if strings.TrimSpace(opts.Panel) == "" {
		return nil, fmt.Errorf("bootstrap: empty panel input")
	}

	t0 := time.Now()
	prompt := buildBootstrapPrompt(opts.Panel)

	bctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	cmd := exec.CommandContext(bctx, opts.ClaudeBinary, "-p", prompt,
		"--append-system-prompt", "Respond with valid JSON only. No prose, no markdown code fences.")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("claude -p failed: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}

	rawOut := strings.TrimSpace(stdout.String())
	cleaned := stripFences(rawOut)

	var ext Extractor
	if err := json.Unmarshal([]byte(cleaned), &ext); err != nil {
		return nil, fmt.Errorf("parse claude output as JSON: %w (got: %s)", err, truncate(cleaned, 400))
	}
	if err := ext.Validate(); err != nil {
		return nil, fmt.Errorf("validate generated extractor: %w", err)
	}

	// Dry-run: required fields must extract from the panel that bootstrapped them.
	probe := ext.Apply(opts.Panel)
	if len(probe.Missing) > 0 {
		return nil, fmt.Errorf("generated extractor failed dry-run: missing %v", probe.Missing)
	}

	ext.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	ext.GeneratedBy = "claude"

	if err := SaveExtractor(&ext, opts.Panel); err != nil {
		return nil, fmt.Errorf("save extractor: %w", err)
	}
	savedAt, _, _ := ExtractorPaths()

	return &BootstrapInfo{
		Extractor: &ext,
		SavedAt:   savedAt,
		ElapsedS:  round2(time.Since(t0).Seconds()),
	}, nil
}

// Reapply re-runs the extractor against the cleaned panel already captured
// in res. Used after a successful Bootstrap so the same poll can persist the
// freshly extracted values without making a second round trip.
func Reapply(res Result, ext *Extractor) Result {
	res.Extracted = ext.Apply(res.RawFull)
	res.OK = len(res.Extracted.Missing) == 0
	res.ExtractorOrigin = string(OriginUser)
	res.SessionPct = nil
	res.WeekPct = nil
	res.SessionResetRaw = ""
	res.WeekResetRaw = ""
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
	return res
}

const bootstrapPromptHeader = `You are configuring an extraction system for the Claude Code /usage panel.

Below, between the BEGIN/END markers, is the cleaned terminal output of running /usage. Generate extraction rules in this exact JSON schema:

{
  "version": 1,
  "fields": [
    {"name": "<field>", "type": "int" | "string", "regex": "<RE2 regex>", "group": <int>, "required": <bool>}
  ]
}

REQUIRED fields (must extract or the extractor is invalid):
- session_pct (int): the "Current session" percentage value
- week_pct (int): the "Current week (all models)" percentage. Do NOT pick the per-model line (e.g. "Current week (Sonnet only)") if present.

OPTIONAL fields (extract if visible, mark required: false):
- session_reset (string): human-readable reset hint for the session bucket, e.g. "May 1, 1am" or "2am". Capture only the value, NOT the timezone in parentheses.
- week_reset (string): same for the weekly bucket.

CRITICAL constraints:
- Regexes must be RE2-compatible (Go's regexp package). No backreferences, no lookarounds.
- The TUI uses cursor positioning rather than whitespace, so labels often appear run together: "Currentsession" (no space), "Currentweek(allmodels)". Account for this; use \s* not \s+ where whitespace might be missing.
- Use case-insensitive flag (?i) and DOTALL (?s) where appropriate. Use lazy quantifiers (.*?) to avoid over-matching across buckets.
- "group" is the regex capture group whose contents become the field value (1 = first capture, 0 = whole match).

Respond with the JSON object only. No prose. No markdown fences.`

func buildBootstrapPrompt(panel string) string {
	return fmt.Sprintf("%s\n\n--- BEGIN /usage panel output ---\n%s\n--- END /usage panel output ---", bootstrapPromptHeader, panel)
}

var fenceRe = regexp.MustCompile("(?s)^```(?:json)?\\s*\\n?(.*?)\\n?```$")

// stripFences removes a single ```...``` (or ```json...```) wrapper if
// present. Defensive: claude is asked not to use fences but may anyway.
func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if m := fenceRe.FindStringSubmatch(s); m != nil {
		return strings.TrimSpace(m[1])
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
