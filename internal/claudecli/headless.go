// Package claudecli is the single seam through which Bloodhound invokes a
// headless `claude` process. Both the /usage extractor self-heal and the
// model-price heal go through RunHeadless, so there is exactly one place
// that spells out the `claude -p` flags and parses its JSON envelope.
//
// This is deliberately a narrow surface. The plan is to move Bloodhound's
// claude-driving onto the sibling claude-p / pupptyeer library; when that
// happens, RunHeadless (and the interactive pty driver) are the only
// implementations that change, and every caller keeps working.
package claudecli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// HeadlessOptions configures one `claude -p --output-format json` run.
type HeadlessOptions struct {
	// Binary is the resolved path to claude. "" falls back to "claude" on
	// PATH.
	Binary string
	// Prompt is the single -p user message.
	Prompt string
	// SystemPrompt is passed via --append-system-prompt. Omitted when empty.
	SystemPrompt string
	// MCPConfig is passed via --mcp-config. Omitted when empty (a plain
	// lookup with no bridge needs no MCP server).
	MCPConfig string
	// AllowedTools is passed via --allowedTools. Omitted when empty.
	AllowedTools string
	// MaxTurns is passed via --max-turns. Omitted when <= 0.
	MaxTurns int
	// ExtraEnv is appended to the inherited environment (e.g. a bridge
	// socket path the MCP server reads).
	ExtraEnv []string
	// Stderr, if set, receives claude's stderr live.
	Stderr io.Writer
	// Timeout bounds the run on top of ctx. 0 means ctx alone governs.
	Timeout time.Duration
}

// Cost mirrors the token/cost fields in claude -p's JSON envelope. Zero
// value when the envelope is missing or the format drifts.
type Cost struct {
	NumTurns                 int     `json:"num_turns"`
	TotalCostUSD             float64 `json:"total_cost_usd"`
	InputTokens              int     `json:"input_tokens"`
	OutputTokens             int     `json:"output_tokens"`
	CacheReadInputTokens     int     `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int     `json:"cache_creation_input_tokens"`
}

// Result is the parsed outcome of a headless run.
type Result struct {
	// ResultText is the model's final message (the envelope's `result`).
	// This is what a stdout-parsing caller (price heal) reads; a
	// bridge-driven caller (the extractor) ignores it and reads its tool
	// results instead.
	ResultText string
	// Cost is best-effort token/cost telemetry.
	Cost Cost
	// APIError is a human-readable auth/rate-limit/terminal failure pulled
	// from the envelope, or "" on success.
	APIError string
	// Raw is claude's full stdout, for callers wanting more than the above.
	Raw []byte
	// StderrTail is the last few KB of stderr (only if Stderr was a
	// *bytes.Buffer we can read back; otherwise empty).
	StderrTail string
	// DurationMs is wall time for the run.
	DurationMs int64
	// Err is a process-level failure (spawn failed, killed, non-zero exit).
	// A non-nil Err with a populated envelope can still be useful.
	Err error
}

// RunHeadless invokes `claude -p --output-format json` and parses the
// envelope. It never panics on a malformed envelope — a parse miss yields
// zero-valued Cost / empty APIError, and the caller decides what to do with
// ResultText and Err.
func RunHeadless(ctx context.Context, opts HeadlessOptions) Result {
	t0 := time.Now()
	res := Result{}
	defer func() { res.DurationMs = time.Since(t0).Milliseconds() }()

	bin := opts.Binary
	if bin == "" {
		bin = "claude"
	}
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, bin, buildArgs(opts)...)
	cmd.Env = append(os.Environ(), opts.ExtraEnv...)
	if opts.Stderr != nil {
		cmd.Stderr = opts.Stderr
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	res.Err = cmd.Run()
	res.Raw = stdout.Bytes()
	res.ResultText = parseResultText(res.Raw)
	res.Cost = parseCost(res.Raw)
	res.APIError = parseAPIError(res.Raw)
	if buf, ok := opts.Stderr.(*bytes.Buffer); ok {
		res.StderrTail = tailString(buf.String(), 4000)
	}
	return res
}

// buildArgs assembles the claude CLI arguments. Split out so the exact
// flag surface both heals depend on is testable without spawning claude.
// --output-format json is always present; the rest are omitted when unset
// so a plain lookup doesn't pass empty --mcp-config / --append-system-prompt.
func buildArgs(opts HeadlessOptions) []string {
	args := []string{"-p", opts.Prompt, "--output-format", "json"}
	if opts.MCPConfig != "" {
		args = append(args, "--mcp-config", opts.MCPConfig)
	}
	if opts.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", opts.SystemPrompt)
	}
	if opts.MaxTurns > 0 {
		args = append(args, "--max-turns", fmt.Sprintf("%d", opts.MaxTurns))
	}
	if opts.AllowedTools != "" {
		args = append(args, "--allowedTools", opts.AllowedTools)
	}
	return args
}

func parseResultText(stdout []byte) string {
	var raw struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(stdout, &raw); err != nil {
		return ""
	}
	return raw.Result
}

// parseCost pulls cost/token fields from the envelope. Best-effort.
func parseCost(stdout []byte) Cost {
	var raw struct {
		NumTurns     int     `json:"num_turns"`
		TotalCostUSD float64 `json:"total_cost_usd"`
		Usage        struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(stdout, &raw); err != nil {
		return Cost{}
	}
	return Cost{
		NumTurns:                 raw.NumTurns,
		TotalCostUSD:             raw.TotalCostUSD,
		InputTokens:              raw.Usage.InputTokens,
		OutputTokens:             raw.Usage.OutputTokens,
		CacheReadInputTokens:     raw.Usage.CacheReadInputTokens,
		CacheCreationInputTokens: raw.Usage.CacheCreationInputTokens,
	}
}

// parseAPIError inspects the envelope's failure-shape fields.
func parseAPIError(stdout []byte) string {
	var raw struct {
		IsError        bool   `json:"is_error"`
		Subtype        string `json:"subtype"`
		Result         string `json:"result"`
		APIErrorStatus any    `json:"api_error_status"`
		TerminalReason string `json:"terminal_reason"`
	}
	if err := json.Unmarshal(stdout, &raw); err != nil {
		return ""
	}
	if !raw.IsError {
		return ""
	}
	var parts []string
	if raw.Subtype != "" && raw.Subtype != "success" {
		parts = append(parts, raw.Subtype)
	}
	if raw.APIErrorStatus != nil {
		parts = append(parts, fmt.Sprintf("api_error_status=%v", raw.APIErrorStatus))
	}
	if raw.TerminalReason != "" && raw.TerminalReason != "completed" {
		parts = append(parts, "terminal_reason="+raw.TerminalReason)
	}
	if raw.Result != "" {
		r := raw.Result
		if len(r) > 240 {
			r = r[:240] + "…"
		}
		parts = append(parts, r)
	}
	if len(parts) == 0 {
		return "is_error=true (no further details in envelope)"
	}
	return strings.Join(parts, " · ")
}

func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
