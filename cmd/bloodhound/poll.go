package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/PeterSR/claude-code-bloodhound/internal/config"
	"github.com/PeterSR/claude-code-bloodhound/internal/events/sensors"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
	"github.com/PeterSR/claude-code-bloodhound/internal/usage"
)

var (
	pollJSON     bool
	pollTimeoutS int
)

var pollCmd = &cobra.Command{
	Use:   "poll",
	Short: "Drive Claude Code's /usage panel once and persist the observation",
	Long: `Spawns the claude binary in a pty, captures /usage, applies the active
extractor, and persists the result to the local store.

For ongoing observation, prefer ` + "`bloodhound daemon`" + ` — it polls
/usage on its own cadence (default every 5 minutes) and auto-heals the
extractor when the panel layout changes. This subcommand is for one-off
scrapes and for users who don't run the daemon.

If extraction misses required fields, hit the Retrain button on the
Debug page (or POST to /api/extractor/retrain) to let the orchestrator
re-learn the extractor against a fresh capture.`,
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

		// Reconcile here too, so a cron-scheduled install (no daemon, just
		// `bloodhound poll` on a timer) records the same transitions and
		// fires the same one-shots a daemon-scheduled one does.
		if _, err := sensors.Run(ctx, s, time.Now()); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warn: events reconcile: %v\n", err)
		}

		w := cmd.OutOrStdout()
		if pollJSON {
			out := map[string]any{
				"ok":                     res.OK,
				"elapsed_s":              res.ElapsedS,
				"extractor_origin":       res.ExtractorOrigin,
				"session_pct":            res.SessionPct,
				"week_pct":               res.WeekPct,
				"session_reset":          res.SessionResetRaw,
				"week_reset":             res.WeekResetRaw,
				"missing":                res.Extracted.Missing,
				"observation_id":         obs.ID,
				"session_reset_detected": obs.SessionResetDetected,
				"week_reset_detected":    obs.WeekResetDetected,
			}
			if fetchErr != nil {
				out["error"] = fetchErr.Error()
			}
			b, _ := json.MarshalIndent(out, "", "  ")
			fmt.Fprintln(w, string(b))
			return nil
		}

		fmt.Fprintf(w, "scrape %s in %.1fs (observation #%d, extractor: %s)\n",
			okOrFail(res.OK), res.ElapsedS, obs.ID, res.ExtractorOrigin)
		printPct(w, "session", res.SessionPct, res.SessionResetRaw)
		printPct(w, "week (all models)", res.WeekPct, res.WeekResetRaw)
		if len(res.Extracted.Missing) > 0 {
			fmt.Fprintf(w, "  ⚠  required fields missing: %v\n", res.Extracted.Missing)
			fmt.Fprintf(w, "  hint: trigger a retrain from the Debug page or `curl -X POST --unix-socket $XDG_RUNTIME_DIR/bloodhound/api.sock http://bh/api/extractor/retrain`\n")
		}
		if obs.SessionResetDetected {
			fmt.Fprintln(w, "  session bucket reset detected since last poll")
		}
		if obs.WeekResetDetected {
			fmt.Fprintln(w, "  weekly bucket reset detected since last poll")
		}
		if !res.OK {
			return fmt.Errorf("scrape failed extraction")
		}
		return nil
	},
}

func okOrFail(ok bool) string {
	if ok {
		return "ok"
	}
	return "FAILED"
}

func printPct(w interface{ Write([]byte) (int, error) }, label string, pct *int, reset string) {
	if pct == nil {
		fmt.Fprintf(asWriter(w), "  %-20s —\n", label)
		return
	}
	if reset == "" {
		reset = "—"
	}
	fmt.Fprintf(asWriter(w), "  %-20s %3d%%   resets %s\n", label, *pct, reset)
}

// asWriter satisfies io.Writer from a cobra writer interface.
type writerOnly = interface{ Write([]byte) (int, error) }

func asWriter(w writerOnly) writerOnly { return w }

func init() {
	pollCmd.Flags().BoolVar(&pollJSON, "json", false, "emit JSON instead of human text")
	pollCmd.Flags().IntVar(&pollTimeoutS, "timeout", 30, "overall timeout in seconds")
	rootCmd.AddCommand(pollCmd)
}
