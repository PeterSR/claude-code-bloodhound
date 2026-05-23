package selfheal

import (
	"bytes"
	"context"
)

// Mode picks how the orchestrator (the AI that drives the inner pty
// via MCP tools) is invoked. Both modes use the same bridge and the
// same tool surface; they differ only in how the LLM is launched and
// how its lifecycle is observed.
type Mode string

const (
	// ModeInteractive spawns interactive `claude` in its own pty and
	// drives it from Go. Counts against the user's interactive
	// subscription limits (the ones reserved for Claude Code TUI), so
	// it avoids the post-2026-06-15 Agent SDK credit / extra-usage path.
	ModeInteractive Mode = "interactive"

	// ModeHeadless spawns `claude -p` with --output-format=json. Returns
	// rich cost / error info via the JSON envelope, but as of 2026-06-15
	// draws from the Agent SDK $100 credit (and then extra usage) rather
	// than the interactive subscription limits.
	ModeHeadless Mode = "headless"
)

// Orchestrator runs the AI-driven portion of one self-heal attempt
// against an already-prepared bridge socket. Implementations decide
// how the LLM is invoked; both must obey ctx for cancellation.
type Orchestrator interface {
	Run(ctx context.Context, opts OrchestratorOpts) OrchestratorOutcome
}

// OrchestratorOpts is the data both orchestrator implementations need.
// It's deliberately bridge-shaped (a socket path + an MCP config the
// LLM will read) so the same setup serves both modes.
type OrchestratorOpts struct {
	// ClaudeBinary is the resolved path to the `claude` executable.
	ClaudeBinary string

	// BridgeSock is the unix-socket path where the in-process
	// BridgeServer is listening.
	BridgeSock string

	// MCPConfigPath is the on-disk JSON the LLM should read with
	// --mcp-config. Same file for both modes.
	MCPConfigPath string

	// UserPrompt is the single user message that kicks off the heal.
	UserPrompt string

	// SystemPrompt is appended to claude's system prompt via
	// --append-system-prompt.
	SystemPrompt string

	// AllowedTools is the comma-joined list passed to --allowedTools.
	AllowedTools string

	// MaxTurns is honoured by the headless mode via --max-turns. The
	// interactive mode has no equivalent; the timeout watchdog is its
	// only stop condition.
	MaxTurns int

	// Bridge gives the interactive mode a way to observe save_extractor
	// success without polling the filesystem. Headless ignores it.
	Bridge *BridgeServer

	// Stderr receives the orchestrator's stderr in real time. Headless
	// uses it for the daemon log; interactive currently has no separate
	// stderr stream (the pty muxes everything).
	Stderr *bytes.Buffer
}

// OrchestratorOutcome is the per-orchestrator result. Success / failure
// of the *heal as a whole* is decided by the outer Run() function via
// the extractor file mtime — these fields only describe how the LLM
// session itself went.
type OrchestratorOutcome struct {
	// OrchestratorMs is wall time spent inside the LLM session.
	OrchestratorMs int64

	// Cost is populated by the headless orchestrator from `claude -p`'s
	// JSON envelope. Interactive returns a zero value (no equivalent
	// telemetry available without scraping `/cost` from the TUI).
	Cost CostInfo

	// APIError, if non-empty, is a human-readable description of an
	// auth / rate-limit / similar failure surfaced by the LLM session.
	// Headless parses it from -p's JSON; interactive scrapes it from
	// the rendered screen.
	APIError string

	// Err is set when the orchestrator process itself couldn't run
	// (spawn failed, killed by watchdog, etc.). A non-nil Err does not
	// necessarily mean the heal failed — save_extractor may have fired
	// before the process was killed.
	Err error

	// StderrTail is the last few KB of the orchestrator's stderr.
	// Empty for interactive mode.
	StderrTail string

	// OuterSessionID is the --session-id we passed to claude. Set only
	// by the interactive orchestrator (headless -p picks its own).
	OuterSessionID string
}
