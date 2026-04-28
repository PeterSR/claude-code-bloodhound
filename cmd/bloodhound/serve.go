package main

import (
	"errors"

	"github.com/spf13/cobra"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the web UI and JSON API server",
	Long:  `Reads from the local SQLite database and serves the web UI plus its backing API.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return errors.New("serve: not yet implemented")
	},
}

func init() {
	rootCmd.AddCommand(serveCmd)
}
