package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "bloodhound",
	Short: "Track Claude Code subscription usage over time",
	Long: `Bloodhound captures Claude Code 5-hour and weekly quota usage,
classifies how tokens are spent, and surfaces actionable insights so you can
make educated decisions about how to use Claude Code today.`,
	SilenceUsage: true,
}

// exitCodeError asks main for a specific exit status without the usual
// "error:" line. It is for outcomes a script wants to branch on that are not
// failures: the command has already said whatever needed saying, and a status
// other than 0 or 1 lets the caller tell one ordinary outcome from another.
type exitCodeError struct{ code int }

func (e exitCodeError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func main() {
	if err := rootCmd.Execute(); err != nil {
		var ec exitCodeError
		if errors.As(err, &ec) {
			os.Exit(ec.code)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
