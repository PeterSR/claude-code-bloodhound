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
	res.SessionResetTZ = ""
	res.WeekResetTZ = ""
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

REQUIRED fields (every one of these must extract; mark required: true):
- session_pct (int): the "Current session" percentage value.
- session_reset (string): the wall-clock reset time for the session bucket, e.g. "May 1, 1am" or "10:50am". Capture only the time, NOT the timezone.
- session_reset_tz (string): the IANA timezone name in parentheses next to the session reset, e.g. "Europe/Copenhagen", "America/Los_Angeles". Capture only the contents of the parens.
- week_pct (int): the "Current week (all models)" percentage. Do NOT pick the per-model line (e.g. "Current week (Sonnet only)") if present.
- week_reset (string): same as session_reset but for the weekly bucket.
- week_reset_tz (string): the IANA timezone name for the weekly reset.

All six are load-bearing. If any one is missing the panel parse fails loudly and the user is prompted to re-bootstrap; that's better than silently storing wrong / missing data.

The timezone is essential — without it, "10:50am" is ambiguous and resolves to the wrong UTC instant.

CRITICAL constraints:
- Regexes must be RE2-compatible (Go's regexp package). No backreferences, no lookarounds.
- The TUI uses cursor positioning rather than whitespace, so labels often appear run together: "Currentsession" (no space), "Currentweek(allmodels)". Account for this; use \s* not \s+ where whitespace might be missing.
- Cursor positioning can also overwrite mid-word: the session reset line sometimes appears as "Reses10:50am" with the 't' missing. Don't rely on the literal word "Resets" matching — match a permissive prefix like "Rese(?:s|ts?)?" before the time so a typo'd line still parses.
- Each bucket's regex must NOT match content from a different bucket. Anchor each regex on its own bucket's "Current X" header and use lazy quantifiers, but if a bucket's reset line is too garbled to match, prefer a no-match (which surfaces as a loud failure) over silently picking up another bucket's reset line.
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
