package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/PeterSR/claude-code-bloodhound/internal/config"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
	"github.com/PeterSR/claude-code-bloodhound/internal/usage"
)

var (
	pollJSON       bool
	pollTimeoutS   int
)

var pollCmd = &cobra.Command{
	Use:   "poll",
	Short: "Drive Claude Code's /usage panel once and persist the observation",
	Long: `Spawns the claude binary in a pty, types /usage, captures the rendered
panel, and writes a structured observation (plus a raw dump for debugging) to
the local SQLite store. Reset detection runs against the prior observation.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(
			context.Background(),
			time.Duration(pollTimeoutS)*time.Second,
		)
		defer cancel()

		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("config: %w", err)
		}

		s, err := store.Open(ctx)
		if err != nil {
			return err
		}
		defer s.Close()

		res, fetchErr := usage.Fetch(ctx, usage.Options{
			ClaudeBinary: cfg.ClaudeBinary,
			Timeout:      time.Duration(pollTimeoutS-3) * time.Second,
		})

		obs, recErr := s.RecordUsage(ctx, res, fetchErr)
		if recErr != nil {
			return fmt.Errorf("record: %w", recErr)
		}

		w := cmd.OutOrStdout()
		if pollJSON {
			out := map[string]any{
				"ok":               res.OK,
				"elapsed_s":        res.ElapsedS,
				"buckets":          res.Buckets,
				"observation_id":   obs.ID,
				"session_reset":    obs.SessionResetDetected,
				"week_reset":       obs.WeekResetDetected,
			}
			if fetchErr != nil {
				out["error"] = fetchErr.Error()
			}
			b, _ := json.MarshalIndent(out, "", "  ")
			fmt.Fprintln(w, string(b))
			return nil
		}

		if !res.OK {
			fmt.Fprintf(w, "scrape FAILED in %.1fs\n", res.ElapsedS)
			if fetchErr != nil {
				fmt.Fprintf(w, "  error: %v\n", fetchErr)
			}
			fmt.Fprintf(w, "  raw dump persisted (observation #%d) for debugging\n", obs.ID)
			return fmt.Errorf("scrape failed")
		}

		fmt.Fprintf(w, "scrape ok in %.1fs (observation #%d)\n", res.ElapsedS, obs.ID)
		for _, b := range res.Buckets {
			reset := b.ResetRaw
			if reset == "" {
				reset = "—"
			}
			fmt.Fprintf(w, "  %-32s %3d%%   resets %s\n", b.Label, b.Pct, reset)
		}
		if obs.SessionResetDetected {
			fmt.Fprintln(w, "  ⚠  session bucket reset detected since last poll")
		}
		if obs.WeekResetDetected {
			fmt.Fprintln(w, "  ⚠  weekly bucket reset detected since last poll")
		}
		return nil
	},
}

func init() {
	pollCmd.Flags().BoolVar(&pollJSON, "json", false, "emit JSON instead of human text")
	pollCmd.Flags().IntVar(&pollTimeoutS, "timeout", 30, "overall timeout in seconds (must exceed scraper budget)")
	rootCmd.AddCommand(pollCmd)
}
