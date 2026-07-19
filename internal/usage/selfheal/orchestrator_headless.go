package selfheal

import (
	"context"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/claudecli"
)

// headlessOrchestrator runs the heal via `claude -p` with --output-format
// json. This is the pre-2026-06-15 default; after that date its tokens
// draw from the Agent SDK $100 credit (and then extra usage) rather than
// the interactive subscription limits.
//
// The actual invocation goes through claudecli.RunHeadless — the one seam
// Bloodhound uses to spawn a headless claude, shared with the price heal so
// there is a single place that spells out the flags and parses the
// envelope. The extractor reads its result via the bridge (save_extractor),
// so it ignores RunHeadless's ResultText and uses only the cost/error
// telemetry.
type headlessOrchestrator struct{}

func (headlessOrchestrator) Run(ctx context.Context, opts OrchestratorOpts) OrchestratorOutcome {
	t0 := time.Now()

	r := claudecli.RunHeadless(ctx, claudecli.HeadlessOptions{
		Binary:       opts.ClaudeBinary,
		Prompt:       opts.UserPrompt,
		SystemPrompt: opts.SystemPrompt,
		MCPConfig:    opts.MCPConfigPath,
		AllowedTools: opts.AllowedTools,
		MaxTurns:     opts.MaxTurns,
		ExtraEnv:     []string{"BLOODHOUND_SELFHEAL_SOCK=" + opts.BridgeSock},
		Stderr:       opts.Stderr,
	})

	return OrchestratorOutcome{
		OrchestratorMs: time.Since(t0).Milliseconds(),
		Cost: CostInfo{
			NumTurns:                 r.Cost.NumTurns,
			TotalCostUSD:             r.Cost.TotalCostUSD,
			InputTokens:              r.Cost.InputTokens,
			OutputTokens:             r.Cost.OutputTokens,
			CacheReadInputTokens:     r.Cost.CacheReadInputTokens,
			CacheCreationInputTokens: r.Cost.CacheCreationInputTokens,
		},
		APIError:   r.APIError,
		StderrTail: r.StderrTail,
		Err:        r.Err,
	}
}
