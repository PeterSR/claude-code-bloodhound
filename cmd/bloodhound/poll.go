package main

import (
	"errors"

	"github.com/spf13/cobra"
)

var pollCmd = &cobra.Command{
	Use:   "poll",
	Short: "Drive Claude Code's /usage panel once and persist the observation",
	RunE: func(cmd *cobra.Command, args []string) error {
		return errors.New("poll: not yet implemented")
	},
}

func init() {
	rootCmd.AddCommand(pollCmd)
}
