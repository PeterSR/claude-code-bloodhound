package main

import (
	"errors"

	"github.com/spf13/cobra"
)

var ingestCmd = &cobra.Command{
	Use:   "ingest",
	Short: "Walk Claude Code's session JSONL files and update the local database",
	RunE: func(cmd *cobra.Command, args []string) error {
		return errors.New("ingest: not yet implemented")
	},
}

func init() {
	rootCmd.AddCommand(ingestCmd)
}
