package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/PeterSR/claude-code-bloodhound/internal/account"
	"github.com/PeterSR/claude-code-bloodhound/internal/config"
	"github.com/PeterSR/claude-code-bloodhound/internal/events/sensors"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
	"github.com/PeterSR/claude-code-bloodhound/internal/usage"
)

var (
	pollJSON      bool
	pollTimeoutS  int
	pollClaudeDir string
	pollSource    string
)

var pollCmd = &cobra.Command{
	Use:   "poll",
	Short: "Read /usage once and persist the observation",
	Long: `Reads the account's usage once and persists the result to the local store.

By default (usage_source "auto") it asks the OAuth usage endpoint the /usage
panel renders from, using the token Claude Code keeps in the config dir. When
the endpoint cannot answer it falls back to spawning the claude binary in a
pty, capturing the /usage panel and applying the active extractor.
--source api or --source pty forces one path with no fallback, which is how
to check that each still works.

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

		dir := pollClaudeDir
		if dir == "" {
			dirs, err := config.ClaudeConfigDirs(cfg)
			if err != nil {
				return err
			}
			dir = dirs[0]
		} else if dir, err = filepath.Abs(dir); err != nil {
			return err
		}
		accountID, ident, err := account.Observe(ctx, s, dir, time.Now())
		if err != nil {
			return fmt.Errorf("account: %w", err)
		}
		if !ident.OAuth {
			return fmt.Errorf("%s has no subscription login, so it has no /usage meter", dir)
		}

		source := cfg.UsageSource
		if pollSource != "" {
			source = pollSource
		}
		if !usage.ValidMode(source) {
			return fmt.Errorf("unknown source %q (want auto, api or pty)", source)
		}
		res, fallback, fetchErr := usage.Collect(ctx, usage.CollectOptions{
			Mode: source,
			API:  usage.APIOptions{ConfigDir: dir},
			PTY: usage.Options{
				ClaudeBinary: cfg.ClaudeBinary,
				Timeout:      time.Duration(pollTimeoutS-3) * time.Second,
				ConfigDir:    dir,
			},
		})

		obs, recErr := s.RecordUsage(ctx, accountID, res, fetchErr)
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
				"source":                 res.Source,
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
			if fallback != nil {
				out["fallback_reason"] = fallback.Error()
			}
			b, _ := json.MarshalIndent(out, "", "  ")
			fmt.Fprintln(w, string(b))
			return nil
		}

		if fallback != nil {
			fmt.Fprintf(w, "usage endpoint unavailable (%v), read the /usage panel instead\n", fallback)
		}
		if res.Source == usage.SourcePTY {
			fmt.Fprintf(w, "pty scrape %s in %.1fs (observation #%d, extractor: %s)\n",
				okOrFail(res.OK), res.ElapsedS, obs.ID, res.ExtractorOrigin)
		} else {
			fmt.Fprintf(w, "%s read %s in %.1fs (observation #%d)\n",
				res.Source, okOrFail(res.OK), res.ElapsedS, obs.ID)
		}
		if fetchErr != nil {
			fmt.Fprintf(w, "  error: %v\n", fetchErr)
		}
		printPct(w, "session", res.SessionPct, resetLabel(res.SessionResetAt, res.SessionResetRaw))
		printPct(w, "week (all models)", res.WeekPct, resetLabel(res.WeekResetAt, res.WeekResetRaw))
		if len(res.Extracted.Missing) > 0 && res.Source == usage.SourcePTY {
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
			return fmt.Errorf("%s read failed", res.Source)
		}
		return nil
	},
}

// resetLabel prefers the exact instant (the api's) in local time, and falls
// back to the panel's own wording.
func resetLabel(at *time.Time, raw string) string {
	if at != nil {
		return at.Local().Format("Jan 2 15:04")
	}
	return raw
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
	pollCmd.Flags().StringVar(&pollSource, "source", "",
		"auto | api | pty (default: the usage_source config key, itself \"auto\")")
	pollCmd.Flags().StringVar(&pollClaudeDir, "claude-dir", "",
		"Claude Code config dir whose account to scrape (default: the first of claude_dirs)")
	rootCmd.AddCommand(pollCmd)
}
