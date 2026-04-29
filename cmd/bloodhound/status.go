package main

import (
	"context"
	"fmt"
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
keystroke without touching the network.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Tight timeout — statusline is on a hot path.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		cfg, err := config.Load()
		if err != nil {
			fmt.Fprintln(cmd.OutOrStdout(), cfg.StatuslinePrefix+" config error")
			return nil
		}
		s, err := store.Open(ctx)
		if err != nil {
			fmt.Fprintln(cmd.OutOrStdout(), cfg.StatuslinePrefix+" db error")
			return nil
		}
		defer s.Close()
		fmt.Fprintln(cmd.OutOrStdout(), statusline.Render(ctx, cfg, s, time.Now()))
		return nil
	},
}

func init() {
	rootCmd.AddCommand(statusCmd)
}
