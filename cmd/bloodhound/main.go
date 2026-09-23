package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

var rootCmd = &cobra.Command{
	Use:   "bloodhound",
	Short: "Track Claude Code subscription usage over time",
	Long: `Bloodhound captures Claude Code 5-hour and weekly quota usage,
classifies how tokens are spent, and surfaces actionable insights so you can
make educated decisions about how to use Claude Code today.`,
	SilenceUsage: true,
}

// accountFlag is the global --account: which Claude account's meter a
// command reads. 0 means the primary account (whoever the first of
// claude_dirs is logged in to).
var accountFlag int64

func init() {
	rootCmd.PersistentFlags().Int64Var(&accountFlag, "account", 0,
		"account id whose meter to read (see `bloodhound doctor`); default the primary account")
}

// cliAccount resolves --account against the store.
func cliAccount(ctx context.Context, s *store.Store) (int64, error) {
	if accountFlag > 0 {
		return accountFlag, nil
	}
	return s.PrimaryAccountID(ctx)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
