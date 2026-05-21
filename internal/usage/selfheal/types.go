// Package selfheal implements the phase 2 self-heal path: an
// orchestrator claude-p that drives a live pty (spawned by the daemon
// itself) via four MCP tools — read_pty, send_keys, test_regex,
// save_extractor.
//
// The daemon owns the pty lifecycle (spawn, kill, cleanup, watchdog).
// claude owns the decision-making: when to type, when to test a
// pattern, when to save. The Go side stays minimal — no hard-coded
// trust-modal detection, no prompt-byte heuristics. The orchestrator
// reads what's on screen and acts.
//
// File layout:
//   types.go    — shared request/result types for the four tools
//   session.go  — Session: live pty + frozen target extractor schema
//                 + the four tool implementations.
//   server.go   — wires the Session to an MCP server (stdio bridge)
//   run.go      — Run: entry point. spawns inner claude, starts the
//                 bridge, launches the orchestrator, watchdogs.
package selfheal

// ReadPTYRequest controls how long read_pty waits for the pty to be
// quiet before snapshotting. SettleMs is "no new bytes for this long".
// Zero means snapshot immediately.
type ReadPTYRequest struct {
	SettleMs int `json:"settle_ms"`
}

// ReadPTYResult is what read_pty returns to claude.
type ReadPTYResult struct {
	// Grid is the VT-rendered terminal contents at snapshot time. Rows
	// joined with \n; trailing whitespace per row trimmed.
	Grid string `json:"grid"`
	// Cols and Rows describe the virtual terminal dimensions.
	Cols int `json:"cols"`
	Rows int `json:"rows"`
	// Quiet is true if the pty was idle for SettleMs before snapshot,
	// false if SettleMs elapsed without quiet (still actively rendering).
	Quiet bool `json:"quiet"`
}

// SendKeysRequest is the text to write into the pty. Use \r for Enter,
// \x03 for Ctrl-C. No interpretation beyond what claude provides.
type SendKeysRequest struct {
	Text string `json:"text"`
}

// SendKeysResult reports how many bytes made it to the pty.
type SendKeysResult struct {
	Bytes int `json:"bytes"`
}

// TestRegexRequest asks the server to compile + run a pattern against
// the current rendered grid.
type TestRegexRequest struct {
	Pattern string `json:"pattern"`
	// Group is the capture group whose value to return (0 = whole match).
	Group int `json:"group"`
	// Field is just a label echoed back in the response; helps the
	// orchestrator keep track of what each test was for.
	Field string `json:"field"`
}

// TestRegexResult is the outcome.
type TestRegexResult struct {
	// Field echoes the request label.
	Field string `json:"field"`
	// Compiled is true iff the pattern is a valid RE2 expression.
	Compiled bool `json:"compiled"`
	// CompileError is populated when Compiled=false.
	CompileError string `json:"compile_error,omitempty"`
	// Matched is true iff the pattern matched the current grid.
	Matched bool `json:"matched"`
	// Value is the contents of the requested capture group (only
	// populated when Matched=true).
	Value string `json:"value,omitempty"`
	// Context is ±60 chars around the match, useful for the
	// orchestrator to reason about over- or undershoot.
	Context string `json:"context,omitempty"`
}

// SaveExtractorRequest carries the orchestrator's proposed rules.
// The server validates the schema, dry-runs against the current grid,
// and only persists when every required field extracts.
type SaveExtractorRequest struct {
	Fields []FieldRule `json:"fields"`
}

// FieldRule is one rule in the extractor. Mirrors usage.Extractor's
// field shape so the orchestrator can think in our extractor DSL
// directly.
type FieldRule struct {
	Name     string `json:"name"`
	Type     string `json:"type"` // "int" | "string"
	Regex    string `json:"regex"`
	Group    int    `json:"group"`
	Required bool   `json:"required"`
}

// SaveExtractorResult is what comes back. Ok=true means the rules
// passed dry-run and were persisted.
type SaveExtractorResult struct {
	Ok bool `json:"ok"`
	// SavedAt is the absolute path the extractor was written to
	// (when Ok=true).
	SavedAt string `json:"saved_at,omitempty"`
	// Missing lists the required field names that failed to extract
	// against the current grid (when Ok=false). Empty when Ok=true.
	Missing []string `json:"missing,omitempty"`
	// Error is populated for schema / compile failures the orchestrator
	// should fix and retry.
	Error string `json:"error,omitempty"`
}
