package selfheal

import (
	"context"
	"fmt"
	"time"
)

// interactiveOrchestrator drives a second interactive `claude` session
// (in addition to the inner one being scraped) via pty. The orchestrator
// reaches the bridge through MCP tools, same as the headless mode, but
// its tokens count against the user's interactive subscription limits
// rather than the Agent SDK credit / extra-usage pool.
type interactiveOrchestrator struct{}

func (interactiveOrchestrator) Run(ctx context.Context, opts OrchestratorOpts) OrchestratorOutcome {
	t0 := time.Now()
	sessionID := NewSessionID()

	cs, err := LaunchClaude(ctx, ClaudeLaunch{
		Binary:             opts.ClaudeBinary,
		MCPConfig:          opts.MCPConfigPath,
		StrictMCPConfig:    true,
		AllowedTools:       opts.AllowedTools,
		AppendSystemPrompt: opts.SystemPrompt,
		// acceptEdits leaves Edit/Write auto-approved; combined with
		// --allowedTools naming our bridge tools explicitly, the LLM
		// shouldn't see any "Allow this tool?" prompts. We avoid
		// bypassPermissions deliberately — it's equivalent to
		// --dangerously-skip-permissions and we don't want that
		// running on a user's machine implicitly.
		PermissionMode: "acceptEdits",
		SessionID:      sessionID,
	})
	if err != nil {
		return OrchestratorOutcome{
			OrchestratorMs: time.Since(t0).Milliseconds(),
			OuterSessionID: sessionID,
			Err:            err,
		}
	}
	defer cs.Close()

	// Generous startup budget: cold claude can take a few seconds to
	// reach its prompt under load, and the trust modal eats time too.
	if err := cs.WaitForReady(ctx, 20*time.Second); err != nil {
		apiErr := ClassifyInteractiveFailure(cs.Snapshot())
		return OrchestratorOutcome{
			OrchestratorMs: time.Since(t0).Milliseconds(),
			OuterSessionID: sessionID,
			Err:            fmt.Errorf("wait for input prompt: %w", err),
			APIError:       apiErr,
		}
	}

	if err := cs.SendPrompt(opts.UserPrompt); err != nil {
		return OrchestratorOutcome{
			OrchestratorMs: time.Since(t0).Milliseconds(),
			OuterSessionID: sessionID,
			Err:            err,
		}
	}

	// Wait for save_extractor to succeed (via the bridge channel),
	// claude to exit on its own, or ctx to time out. Budget=0 means
	// "let ctx be the only ceiling" — the outer Run() sets a 90s
	// default and the manual retrain path bumps it to 150s.
	var done <-chan struct{}
	if opts.Bridge != nil {
		done = opts.Bridge.Saved()
	}
	waitErr := cs.WaitForDone(ctx, done, 0)

	outcome := OrchestratorOutcome{OrchestratorMs: time.Since(t0).Milliseconds(), OuterSessionID: sessionID}
	if waitErr != nil {
		outcome.Err = waitErr
		outcome.APIError = ClassifyInteractiveFailure(cs.Snapshot())
	} else {
		// Success path: ask claude to exit cleanly so we don't leave
		// an orphaned spinner mid-render. cs.Close() in the deferred
		// branch is the hard backstop.
		cs.Exit()
	}
	return outcome
}
