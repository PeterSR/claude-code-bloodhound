package claudecli

import (
	"strings"
	"testing"
)

func joined(args []string) string { return strings.Join(args, " ") }

func TestBuildArgsPriceHealShape(t *testing.T) {
	// Price heal: web search, a turn cap, no bridge — so no --mcp-config and
	// no --append-system-prompt should appear.
	args := buildArgs(HeadlessOptions{
		Prompt:       "find the price",
		AllowedTools: "WebSearch,WebFetch",
		MaxTurns:     8,
	})
	got := joined(args)
	for _, want := range []string{
		"-p find the price",
		"--output-format json",
		"--allowedTools WebSearch,WebFetch",
		"--max-turns 8",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "--mcp-config") {
		t.Errorf("price heal should not pass --mcp-config: %q", got)
	}
	if strings.Contains(got, "--append-system-prompt") {
		t.Errorf("price heal should not pass --append-system-prompt: %q", got)
	}
}

func TestBuildArgsExtractorShape(t *testing.T) {
	// Extractor heal: bridge-driven, so it carries an MCP config and a
	// system prompt.
	args := buildArgs(HeadlessOptions{
		Prompt:       "drive the pty",
		SystemPrompt: "you are an orchestrator",
		MCPConfig:    "/tmp/mcp.json",
		AllowedTools: "mcp__x__read_pty",
		MaxTurns:     25,
	})
	got := joined(args)
	for _, want := range []string{
		"--mcp-config /tmp/mcp.json",
		"--append-system-prompt you are an orchestrator",
		"--max-turns 25",
		"--output-format json",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

func TestBuildArgsOmitsEmptyOptionals(t *testing.T) {
	// The minimal shape: only -p and --output-format json.
	args := buildArgs(HeadlessOptions{Prompt: "hi"})
	if joined(args) != "-p hi --output-format json" {
		t.Errorf("minimal args = %q", joined(args))
	}
}

func TestParseHelpers(t *testing.T) {
	env := `{"is_error": false, "result": "the answer", "num_turns": 3, "total_cost_usd": 0.05,
	         "usage": {"input_tokens": 100, "output_tokens": 20}}`
	if got := parseResultText([]byte(env)); got != "the answer" {
		t.Errorf("result text = %q", got)
	}
	c := parseCost([]byte(env))
	if c.TotalCostUSD != 0.05 || c.NumTurns != 3 || c.InputTokens != 100 {
		t.Errorf("cost = %+v", c)
	}
	if got := parseAPIError([]byte(env)); got != "" {
		t.Errorf("no error expected, got %q", got)
	}

	errEnv := `{"is_error": true, "subtype": "error_max_turns", "result": "hit the cap"}`
	if got := parseAPIError([]byte(errEnv)); !strings.Contains(got, "error_max_turns") {
		t.Errorf("api error = %q", got)
	}

	if parseResultText([]byte("not json")) != "" {
		t.Error("malformed envelope should yield empty result, not panic")
	}
}
