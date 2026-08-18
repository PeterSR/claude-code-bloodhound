package main

import (
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

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
