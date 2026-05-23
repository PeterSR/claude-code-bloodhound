package selfheal

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// headlessOrchestrator runs the heal via `claude -p` with --output-format
// json. This is the pre-2026-06-15 default; after that date its tokens
// draw from the Agent SDK $100 credit (and then extra usage) rather than
// the interactive subscription limits.
type headlessOrchestrator struct{}

func (headlessOrchestrator) Run(ctx context.Context, opts OrchestratorOpts) OrchestratorOutcome {
	t0 := time.Now()

	args := []string{
		"-p", opts.UserPrompt,
		"--mcp-config", opts.MCPConfigPath,
		"--append-system-prompt", opts.SystemPrompt,
		"--max-turns", fmt.Sprintf("%d", opts.MaxTurns),
		"--allowedTools", opts.AllowedTools,
		// JSON output gives us the cost / token info to surface back
		// to the user — they're paying for this turn.
		"--output-format", "json",
	}
	cmd := exec.CommandContext(ctx, opts.ClaudeBinary, args...)
	cmd.Env = append(os.Environ(),
		"BLOODHOUND_SELFHEAL_SOCK="+opts.BridgeSock,
	)
	if opts.Stderr != nil {
		cmd.Stderr = opts.Stderr
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	runErr := cmd.Run()
	outcome := OrchestratorOutcome{
		OrchestratorMs: time.Since(t0).Milliseconds(),
		Cost:           parseCostFromOrchestratorOutput(stdout.Bytes()),
		APIError:       parseAPIErrorFromOrchestratorOutput(stdout.Bytes()),
		Err:            runErr,
	}
	if opts.Stderr != nil {
		outcome.StderrTail = tailString(opts.Stderr.String(), 4000)
	}
	return outcome
}
