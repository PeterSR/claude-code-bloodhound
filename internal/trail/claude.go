package trail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/usage/selfheal"
)

// claudeAnalyzer drives `claude` to produce a brief, reusing the
// self-heal launch machinery (interactive pty or headless -p) and the
// same MCP-bridge pattern: a save_trail_brief tool whose handler
// captures the structured result. Mode mirrors self_heal_mode.
type claudeAnalyzer struct {
	ClaudeBinary string
	Mode         string // "interactive" | "headless"
	Cwd          string // working dir for the analyzer claude
	ProjectsDir  string // for locating the analyzer's JSONL (interactive cost)
	Timeout      time.Duration
}

// Analyze launches the analyzer, waits for save_trail_brief, and returns
// the captured args + cost.
func (a claudeAnalyzer) Analyze(ctx context.Context, in Input) (Result, error) {
	bin := a.ClaudeBinary
	if bin == "" {
		bin = "claude"
	}
	timeout := a.Timeout
	if timeout == 0 {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	bridge, err := newBridgeServer()
	if err != nil {
		return Result{}, fmt.Errorf("bridge: %w", err)
	}
	defer bridge.Close()
	go func() { _ = bridge.Serve() }()

	mcpPath, err := writeTempMCPConfig(buildMCPConfig(bridge.Path()))
	if err != nil {
		return Result{}, err
	}
	defer os.Remove(mcpPath)

	prompt := buildPrompt(in)
	sessionID := in.AnalyzerSessionID
	if sessionID == "" {
		sessionID = selfheal.NewSessionID()
	}

	var res Result
	if a.Mode == "headless" {
		res, err = a.runHeadless(ctx, bin, mcpPath, bridge, prompt, sessionID)
	} else {
		res, err = a.runInteractive(ctx, bin, mcpPath, bridge, prompt, sessionID)
	}
	res.AnalyzerSessionID = sessionID
	if err != nil {
		return res, err
	}
	if bridge.Brief() == nil {
		return res, fmt.Errorf("analyzer finished without calling %s", ToolSaveBrief)
	}
	res.Args = *bridge.Brief()
	return res, nil
}

func (a claudeAnalyzer) runInteractive(ctx context.Context, bin, mcpPath string, bridge *bridgeServer, prompt, sessionID string) (Result, error) {
	cs, err := selfheal.LaunchClaude(ctx, selfheal.ClaudeLaunch{
		Binary:             bin,
		MCPConfig:          mcpPath,
		StrictMCPConfig:    true,
		AllowedTools:       AllowedTools(),
		AppendSystemPrompt: systemPrompt,
		PermissionMode:     "acceptEdits",
		SessionID:          sessionID,
		Cwd:                a.Cwd,
	})
	if err != nil {
		return Result{}, err
	}
	defer cs.Close()

	if err := cs.WaitForReady(ctx, 20*time.Second); err != nil {
		return Result{},
			fmt.Errorf("wait for input prompt: %w (%s)", err, selfheal.ClassifyInteractiveFailure(cs.Snapshot()))
	}
	if err := cs.SendPrompt(prompt); err != nil {
		return Result{}, err
	}
	if err := cs.WaitForDone(ctx, bridge.Saved(), 0); err != nil {
		return Result{},
			fmt.Errorf("wait for save: %w (%s)", err, selfheal.ClassifyInteractiveFailure(cs.Snapshot()))
	}
	cs.Exit()

	// Cost: sum the analyzer's own JSONL token usage (we skip ingesting
	// it, so this is the only attribution path in interactive mode).
	cost := Cost{}
	if a.ProjectsDir != "" {
		if p := LocateSessionJSONL(a.ProjectsDir, sessionID); p != "" {
			cost = sumUsage(p)
		}
	}
	return Result{Cost: cost}, nil
}

func (a claudeAnalyzer) runHeadless(ctx context.Context, bin, mcpPath string, bridge *bridgeServer, prompt, sessionID string) (Result, error) {
	args := []string{
		"-p", prompt,
		"--session-id", sessionID,
		"--mcp-config", mcpPath,
		"--strict-mcp-config",
		"--append-system-prompt", systemPrompt,
		"--allowedTools", AllowedTools(),
		"--output-format", "json",
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), "BLOODHOUND_TRAIL_SOCK="+bridge.Path())
	if a.Cwd != "" {
		cmd.Dir = a.Cwd
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	// The tool fires during the run; if it did, we have a brief and treat
	// a non-zero exit as non-fatal. Only error out if nothing was saved.
	cost := parseHeadlessCost(stdout.Bytes())
	if bridge.Brief() == nil && runErr != nil {
		return Result{Cost: cost}, fmt.Errorf("claude -p: %w: %s", runErr, tail(stderr.String(), 400))
	}
	return Result{Cost: cost}, nil
}

// --- MCP config ------------------------------------------------------

func buildMCPConfig(sock string) string {
	exe, err := os.Executable()
	if err != nil {
		exe = "bloodhound"
	}
	return fmt.Sprintf(`{
  "mcpServers": {
    %q: {
      "command": %q,
      "args": ["_trail_mcp"],
      "env": { "BLOODHOUND_TRAIL_SOCK": %q }
    }
  }
}`, MCPServerName, exe, sock)
}

func writeTempMCPConfig(content string) (string, error) {
	f, err := os.CreateTemp("", "bh-trail-mcp-*.json")
	if err != nil {
		return "", fmt.Errorf("create mcp config: %w", err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// --- cost helpers ----------------------------------------------------

func parseHeadlessCost(stdout []byte) Cost {
	var raw struct {
		TotalCostUSD float64 `json:"total_cost_usd"`
		Usage        struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(stdout, &raw); err != nil {
		return Cost{}
	}
	return Cost{
		CostUSD:           raw.TotalCostUSD,
		InputTokens:       raw.Usage.InputTokens,
		OutputTokens:      raw.Usage.OutputTokens,
		CacheReadTokens:   raw.Usage.CacheReadInputTokens,
		CacheCreateTokens: raw.Usage.CacheCreationInputTokens,
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
