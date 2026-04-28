package main

import (
	"errors"

	"github.com/spf13/cobra"
)

var aggregateCmd = &cobra.Command{
	Use:   "aggregate",
	Short: "Recompute rolling metrics and materialized views",
	RunE: func(cmd *cobra.Command, args []string) error {
		return errors.New("aggregate: not yet implemented")
	},
}

func init() {
	rootCmd.AddCommand(aggregateCmd)
}
