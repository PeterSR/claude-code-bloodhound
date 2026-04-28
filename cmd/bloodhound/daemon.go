package main

import (
	"errors"

	"github.com/spf13/cobra"
)

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Run an in-process scheduler for poll/ingest/aggregate (alternative to OS service)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return errors.New("daemon: not yet implemented")
	},
}

func init() {
	rootCmd.AddCommand(daemonCmd)
}
