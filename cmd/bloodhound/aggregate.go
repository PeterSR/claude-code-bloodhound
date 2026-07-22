package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/PeterSR/claude-code-bloodhound/internal/aggregate"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

var aggregateJSON bool

var aggregateCmd = &cobra.Command{
	Use:   "aggregate",
	Short: "Recompute rolling metrics and materialized views",
	Long: `Rebuilds the sessions, buckets, calibration and limit-attribution
tables from the raw turns + compactions data. Idempotent and cheap (~50k turns runs in well under a
second). Run on a slower cadence than ingest itself.

Mostly useful when you're not running ` + "`bloodhound daemon`" + ` — the daemon
runs aggregation on its own cadence (default every 15 minutes). Running
this alongside the daemon is harmless but redundant.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		s, err := store.Open(ctx)
		if err != nil {
			return err
		}
		defer s.Close()

		stats, err := aggregate.Run(ctx, s, aggregate.Options{})
		w := cmd.OutOrStdout()
		if aggregateJSON {
			b, _ := json.MarshalIndent(stats, "", "  ")
			fmt.Fprintln(w, string(b))
			return err
		}
		fmt.Fprintf(w, "aggregate: %d sessions, %d buckets, %d cal-points, %d attribution rows in %.2fs\n",
			stats.SessionsRefreshed, stats.BucketsRebuilt, stats.CalibrationPointsBuilt,
			stats.AttributionRowsBuilt, stats.ElapsedS)
		return err
	},
}

func init() {
	aggregateCmd.Flags().BoolVar(&aggregateJSON, "json", false, "emit JSON stats")
	rootCmd.AddCommand(aggregateCmd)
}
