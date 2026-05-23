package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/PeterSR/claude-code-bloodhound/internal/config"
	"github.com/PeterSR/claude-code-bloodhound/internal/usage/selfheal"
)

// selfhealTestCmd runs one heal attempt directly and dumps the bridge
// trace plus the persisted JSONL transcripts from both the inner and
// outer claude sessions. Hidden from --help; it's a developer affordance
// for verifying the interactive-orchestrator wiring end-to-end.
var (
	selfhealTestMode    string
	selfhealTestTimeout int
)

var selfhealTestCmd = &cobra.Command{
	Use:    "_selfheal_test",
	Short:  "Run one self-heal attempt and dump full trace (dev only)",
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("config: %w", err)
		}

		mode := cfg.SelfHealMode
		if selfhealTestMode != "" {
			mode = selfhealTestMode
		}
		if mode == "" {
			mode = config.SelfHealModeInteractive
		}

		ctx, cancel := context.WithTimeout(context.Background(),
			time.Duration(selfhealTestTimeout)*time.Second)
		defer cancel()

		// Bridge trace lives in a tempfile so we can show its path.
		tracePath := filepath.Join(os.TempDir(),
			fmt.Sprintf("bh-selfheal-trace-%d.jsonl", time.Now().UnixNano()))
		traceFile, err := os.Create(tracePath)
		if err != nil {
			return fmt.Errorf("create trace: %w", err)
		}
		defer traceFile.Close()

		stderr := &bytes.Buffer{}
		fmt.Fprintf(cmd.OutOrStdout(),
			"running self-heal: mode=%s timeout=%ds trace=%s\n",
			mode, selfhealTestTimeout, tracePath)

		res := selfheal.Run(ctx, selfheal.Options{
			ClaudeBinary: cfg.ClaudeBinary,
			Mode:         selfheal.Mode(mode),
			Timeout:      time.Duration(selfhealTestTimeout-3) * time.Second,
			Stderr:       stderr,
			Trace:        traceFile,
			Force:        true,
		})

		w := cmd.OutOrStdout()
		fmt.Fprintln(w, "")
		fmt.Fprintf(w, "result: ok=%v mode=%s total=%dms orch=%dms\n",
			res.OK, res.Mode, res.TotalMs, res.OrchestratorMs)
		if res.SavedAt != "" {
			fmt.Fprintf(w, "  saved_at:        %s\n", res.SavedAt)
		}
		if res.InnerSessionID != "" {
			fmt.Fprintf(w, "  inner_session:   %s\n", res.InnerSessionID)
		}
		if res.OuterSessionID != "" {
			fmt.Fprintf(w, "  outer_session:   %s\n", res.OuterSessionID)
		}
		if res.Err != nil {
			fmt.Fprintf(w, "  error:           %s\n", res.Err)
		}
		if res.StderrTail != "" {
			fmt.Fprintf(w, "  stderr_tail:\n%s\n", indent(res.StderrTail))
		}

		fmt.Fprintf(w, "\n--- Bridge tool-call trace (%s) ---\n", tracePath)
		dumpFile(w, tracePath, 200)

		inner := locateClaudeJSONL(res.InnerSessionID)
		fmt.Fprintf(w, "\n--- Inner claude JSONL (%s) ---\n", or(inner, "<not found>"))
		if inner != "" {
			dumpFile(w, inner, 200)
		}

		outer := locateClaudeJSONL(res.OuterSessionID)
		fmt.Fprintf(w, "\n--- Outer claude JSONL (%s) ---\n", or(outer, "<not found>"))
		if outer != "" {
			dumpFile(w, outer, 200)
		}

		return nil
	},
}

func init() {
	selfhealTestCmd.Flags().StringVar(&selfhealTestMode, "mode", "",
		"override config: interactive | headless")
	selfhealTestCmd.Flags().IntVar(&selfhealTestTimeout, "timeout", 180,
		"overall timeout in seconds")
	rootCmd.AddCommand(selfhealTestCmd)
}

// locateClaudeJSONL searches ~/.claude/projects/**/<sessionID>.jsonl and
// returns the most recently modified match, or "" if none exists.
func locateClaudeJSONL(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	root := filepath.Join(home, ".claude", "projects")
	var best string
	var bestT time.Time
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(info.Name(), sessionID+".jsonl") {
			return nil
		}
		if info.ModTime().After(bestT) {
			best = path
			bestT = info.ModTime()
		}
		return nil
	})
	return best
}

func dumpFile(w interface{ Write([]byte) (int, error) }, path string, maxLines int) {
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(asWriter(w), "  (read failed: %s)\n", err)
		return
	}
	if len(b) == 0 {
		fmt.Fprintln(asWriter(w), "  (empty)")
		return
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) > maxLines {
		fmt.Fprintf(asWriter(w), "  (showing last %d of %d lines)\n", maxLines, len(lines))
		lines = lines[len(lines)-maxLines:]
	}
	fmt.Fprintln(asWriter(w), strings.Join(lines, "\n"))
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n")
}
