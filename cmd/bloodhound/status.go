package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/PeterSR/claude-code-bloodhound/internal/config"
	"github.com/PeterSR/claude-code-bloodhound/internal/statusline"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Print one statusline summary (consumed by Claude Code's statusline setting)",
	Long: `Reads the latest /usage observation from the local SQLite store and
prints a single line suitable for use as Claude Code's statusline command.

Designed to be cheap (no /usage scrape, just a DB read). Wire it into your
Claude Code settings.json's statusLine.command and it'll fire on every
conversation-state change without touching the network. When Claude Code
pipes its session metadata via stdin, the printed line includes a
per-session alert (cold cache, cache about to expire, /compact recommended)
for that exact session.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Tight timeout — statusline is on a hot path.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		cfg, err := config.Load()
		if err != nil {
			fmt.Fprintln(cmd.OutOrStdout(), cfg.StatuslinePrefix+" config error")
			return nil
		}
		sessionUUID := readSessionUUIDFromStdin()
		s, err := store.Open(ctx)
		if err != nil {
			fmt.Fprintln(cmd.OutOrStdout(), cfg.StatuslinePrefix+" db error")
			return nil
		}
		defer s.Close()
		fmt.Fprintln(cmd.OutOrStdout(), statusline.Render(ctx, cfg, s, time.Now(), sessionUUID))
		return nil
	},
}

// readSessionUUIDFromStdin extracts session_id from the JSON Claude Code
// pipes into the statusline command. Returns "" when stdin is a TTY (the
// user ran `bloodhound status` manually), when the payload isn't JSON,
// or when no session_id is present — the renderer falls back to
// "most-recent active session" in those cases. Best-effort: never errors.
func readSessionUUIDFromStdin() string {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return ""
	}
	// Only read when stdin is a pipe / regular file. A TTY means manual
	// invocation; we'd hang waiting for input.
	if fi.Mode()&os.ModeCharDevice != 0 {
		return ""
	}
	var payload struct {
		SessionID string `json:"session_id"`
	}
	// Cap stdin so a misbehaving caller can't stall the statusline.
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 64*1024)).Decode(&payload); err != nil {
		return ""
	}
	return payload.SessionID
}

func init() {
	rootCmd.AddCommand(statusCmd)
}
