package main

import (
	"errors"

	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Print one statusline summary (consumed by Claude Code's statusline setting)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return errors.New("status: not yet implemented")
	},
}

func init() {
	rootCmd.AddCommand(statusCmd)
}
