package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/PeterSR/claude-code-bloodhound/internal/aggregate"
	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
	"github.com/PeterSR/claude-code-bloodhound/internal/api/server"
	"github.com/PeterSR/claude-code-bloodhound/internal/config"
	"github.com/PeterSR/claude-code-bloodhound/internal/ingest"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
	"github.com/PeterSR/claude-code-bloodhound/internal/trail"
	"github.com/PeterSR/claude-code-bloodhound/internal/usage"
	"github.com/PeterSR/claude-code-bloodhound/internal/usage/selfheal"
)

var (
	daemonPollIntervalS      int
	daemonIngestIntervalS    int
	daemonAggregateIntervalS int
	daemonTrailIntervalS     int
	daemonRunOnce            bool
	daemonLogFile            string
	daemonNoLogFile          bool
	daemonNoAPI              bool
)

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Run the collection scheduler + HTTP API",
	Long: `An alternative to wiring up systemd / launchd / scheduled tasks: a single
long-running process that drives polling, ingestion, and aggregation on
configured intervals (defaults from config.json: poll 5m, ingest 5m,
aggregate 15m). All jobs run sequentially against the shared store, so
SQLite writes never collide.

The daemon exposes a JSON HTTP API on a unix-domain socket at
$XDG_RUNTIME_DIR/bloodhound/api.sock. The bloodhound-gui binary loads
the dashboard and reverse-proxies its requests over that socket. Pass
--no-api to run collection only (no HTTP surface).

By default, daemon output is mirrored to both stdout and a log file at
$XDG_STATE_HOME/bloodhound/daemon.log (or the per-OS state directory on
macOS/Windows). Pass --log-file to override the path, or --no-log-file
to disable file output entirely.

For one-shot CI-style execution that does each job once and exits, pass
--once.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("config: %w", err)
		}

		// Resolve log destination first so startup messages also go there.
		var w io.Writer = cmd.OutOrStdout()
		if !daemonNoLogFile {
			path := daemonLogFile
			if path == "" {
				path, err = config.DaemonLogPath()
				if err != nil {
					return fmt.Errorf("resolve default log path: %w", err)
				}
			}
			if err := config.EnsureDir(filepath.Dir(path)); err != nil {
				return err
			}
			f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return fmt.Errorf("open log file %s: %w", path, err)
			}
			defer f.Close()
			fmt.Fprintf(w, "[daemon] log file: %s\n", path)
			w = io.MultiWriter(w, f)
		}

		pollIvl := durationS(daemonPollIntervalS, cfg.PollIntervalS, 300)
		ingestIvl := durationS(daemonIngestIntervalS, cfg.IngestIntervalS, 300)
		aggIvl := durationS(daemonAggregateIntervalS, cfg.AggregateIntervalS, 900)
		trailIvl := durationS(daemonTrailIntervalS, cfg.TrailIntervalS, 900)

		s, err := store.Open(ctx)
		if err != nil {
			return err
		}
		defer s.Close()

		fmt.Fprintf(w, "[daemon] started %s · poll %s · ingest %s · aggregate %s\n",
			time.Now().UTC().Format(time.RFC3339), pollIvl, ingestIvl, aggIvl)

		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigCh
			fmt.Fprintln(w, "[daemon] shutdown signal received")
			cancel()
		}()

		var wg sync.WaitGroup

		// Bring the API up before the initial cycle. The GUI's setup
		// wizard polls /api/health; if we waited for ingest/aggregate/poll
		// to finish first (the poll alone can take 60s), the wizard would
		// sit on "daemon is down" for the entire initial cycle even
		// though the daemon process is alive.
		if !daemonNoAPI {
			sockPath, err := routes.SocketPath()
			if err != nil {
				return fmt.Errorf("api: %w", err)
			}
			ln, err := server.Listen(sockPath)
			if err != nil {
				return fmt.Errorf("api: %w", err)
			}
			fmt.Fprintf(w, "[daemon] api: unix://%s\n", sockPath)
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := server.Serve(ctx, ln, &server.Server{Store: s}); err != nil {
					fmt.Fprintf(w, "[daemon] api: %v\n", err)
				}
			}()
		}

		// Run each job once at startup (cheapest first so observation
		// percentages persist quickly even on a slow first scrape).
		runIngestOnce(ctx, s, w)
		runAggregateOnce(ctx, s, w)
		runPollOnce(ctx, cfg, s, w)
		if cfg.TrailEnabled {
			runTrailOnce(ctx, cfg, s, w)
		}

		if daemonRunOnce {
			fmt.Fprintln(w, "[daemon] --once: exiting after first cycle")
			return nil
		}

		var mu sync.Mutex
		runUnder := func(fn func()) {
			mu.Lock()
			defer mu.Unlock()
			fn()
		}

		schedule := func(name string, ivl time.Duration, fn func()) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				t := time.NewTicker(ivl)
				defer t.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-t.C:
						runUnder(fn)
					}
				}
			}()
			fmt.Fprintf(w, "[daemon] scheduled %s\n", name)
		}

		schedule("poll", pollIvl, func() { runPollOnce(ctx, cfg, s, w) })
		schedule("ingest", ingestIvl, func() { runIngestOnce(ctx, s, w) })
		schedule("aggregate", aggIvl, func() { runAggregateOnce(ctx, s, w) })
		if cfg.TrailEnabled {
			// Re-read config each tick so toggling trail_mode / window in
			// the UI takes effect without a daemon restart.
			schedule("trail", trailIvl, func() {
				tc, err := config.Load()
				if err != nil {
					tc = cfg
				}
				if !tc.TrailEnabled {
					return
				}
				runTrailOnce(ctx, tc, s, w)
			})
		}

		<-ctx.Done()
		fmt.Fprintln(w, "[daemon] waiting for in-flight jobs")
		wg.Wait()
		fmt.Fprintln(w, "[daemon] stopped")
		return nil
	},
}

func durationS(override, configured, fallback int) time.Duration {
	if override > 0 {
		return time.Duration(override) * time.Second
	}
	if configured > 0 {
		return time.Duration(configured) * time.Second
	}
	return time.Duration(fallback) * time.Second
}

func runPollOnce(ctx context.Context, cfg config.Config, s *store.Store, w io.Writer) {
	if err := ctx.Err(); err != nil {
		return
	}
	// The self-heal path can take ~30-60s on top of the regular ~8s
	// fetch, so size the poll context generously when it's enabled.
	pollBudget := 60 * time.Second
	if cfg.ExtractorSelfHeal {
		pollBudget = 150 * time.Second
	}
	pollCtx, cancel := context.WithTimeout(ctx, pollBudget)
	defer cancel()
	res, fetchErr := usage.Fetch(pollCtx, usage.Options{ClaudeBinary: cfg.ClaudeBinary})

	// Self-heal: if extraction missed required fields, hand the heal
	// off to selfheal.Run — which spawns its own inner claude in a pty
	// and lets an orchestrator claude -p drive it via MCP tools (read_pty,
	// send_keys, test_regex, save_extractor) until the new extractor is
	// validated and persisted. Gated by config so users who want manual
	// control (or who don't want self-heal consuming usage on their
	// account) can flip it off and use the Debug page's manual retrain.
	if cfg.ExtractorSelfHeal && fetchErr == nil && !res.OK {
		fmt.Fprintf(w, "[daemon] poll: extraction missed, starting orchestrator self-heal\n")
		tracePath, traceFile := openSelfHealTrace(w)
		heal := selfheal.Run(pollCtx, selfheal.Options{
			ClaudeBinary: cfg.ClaudeBinary,
			Mode:         selfheal.Mode(cfg.SelfHealMode),
			Timeout:      120 * time.Second,
			Trace:        traceFile,
		})
		if traceFile != nil {
			_ = traceFile.Close()
		}
		if heal.OK {
			fmt.Fprintf(w, "[daemon] poll: self-heal saved a fresh extractor in %dms (saved=%s, trace=%s)\n",
				heal.TotalMs, heal.SavedAt, tracePath)
			// Re-apply against the originally captured panel; if the
			// new regexes still miss (panels differ between heal session
			// and this poll), leave res.OK=false and next poll picks it
			// up naturally.
			if newExt, _, loadErr := usage.LoadExtractor(); loadErr == nil {
				res = usage.Reapply(res, newExt)
			}
		} else {
			fmt.Fprintf(w, "[daemon] poll: self-heal failed (%v) — next poll will retry; manual retrain available from the Debug page; trace=%s\n",
				heal.Err, tracePath)
		}
	}

	obs, err := s.RecordUsage(pollCtx, res, fetchErr)
	switch {
	case err != nil:
		fmt.Fprintf(w, "[daemon] poll: record error %v\n", err)
	case fetchErr != nil && !errors.Is(fetchErr, context.Canceled):
		fmt.Fprintf(w, "[daemon] poll: scrape failed (%v)\n", fetchErr)
	case !res.OK:
		fmt.Fprintf(w, "[daemon] poll: extraction failed (missing %v)\n", res.Extracted.Missing)
	default:
		sess := "?"
		week := "?"
		if res.SessionPct != nil {
			sess = fmt.Sprintf("%d%%", *res.SessionPct)
		}
		if res.WeekPct != nil {
			week = fmt.Sprintf("%d%%", *res.WeekPct)
		}
		fmt.Fprintf(w, "[daemon] poll: ok session=%s week=%s (#%d, %.1fs)\n",
			sess, week, obs.ID, res.ElapsedS)
	}
}

func runIngestOnce(ctx context.Context, s *store.Store, w io.Writer) {
	if err := ctx.Err(); err != nil {
		return
	}
	ictx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	stats, err := ingest.Run(ictx, s, ingest.Options{})
	if err != nil {
		fmt.Fprintf(w, "[daemon] ingest: error %v\n", err)
		return
	}
	fmt.Fprintf(w, "[daemon] ingest: %d/%d files parsed, +%d turns +%d compactions in %.2fs\n",
		stats.FilesParsed, stats.FilesScanned, stats.TurnsAdded, stats.CompactionsAdded, stats.ElapsedS)
}

func runAggregateOnce(ctx context.Context, s *store.Store, w io.Writer) {
	if err := ctx.Err(); err != nil {
		return
	}
	actx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	stats, err := aggregate.Run(actx, s, aggregate.Options{})
	if err != nil {
		fmt.Fprintf(w, "[daemon] aggregate: error %v\n", err)
		return
	}
	fmt.Fprintf(w, "[daemon] aggregate: %d sessions, %d buckets, %d cal-points in %.2fs\n",
		stats.SessionsRefreshed, stats.BucketsRebuilt, stats.CalibrationPointsBuilt, stats.ElapsedS)
}

// runTrailOnce runs one Trail cycle: summarise the recent activity of
// recently-active sessions into per-session briefs. Gated by
// cfg.TrailEnabled at the call sites. Bounded by a generous cycle
// timeout so a slow analyzer can't wedge the scheduler — most cycles
// touch only the 1-2 sessions with new activity (the rest skip cheaply).
func runTrailOnce(ctx context.Context, cfg config.Config, s *store.Store, w io.Writer) {
	if err := ctx.Err(); err != nil {
		return
	}
	tctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()
	st, err := trail.Run(tctx, s, cfg, w)
	if err != nil {
		fmt.Fprintf(w, "[daemon] trail: error %v\n", err)
		return
	}
	fmt.Fprintf(w, "[daemon] trail: %d considered, %d analyzed, %d first-seen, %d skipped, $%.4f, %d errors in %.1fs\n",
		st.Considered, st.Analyzed, st.FirstSeen, st.Skipped, st.CostUSD, len(st.Errors), st.ElapsedS)
	for _, e := range st.Errors {
		fmt.Fprintf(w, "[daemon] trail: · %s\n", e)
	}
}

// openSelfHealTrace returns a writable trace file inside the daemon's
// state dir plus its path. The caller closes it when the heal finishes.
// Trace files accumulate; rotation is the user's problem for now (one
// file per heal, JSONL, named with a unix-nano timestamp). A nil file
// + descriptive path is returned on failure so the heal still runs —
// trace capture is a debugging aid, not load-bearing.
func openSelfHealTrace(w io.Writer) (string, *os.File) {
	dir, err := config.StateDir()
	if err != nil {
		fmt.Fprintf(w, "[daemon] poll: trace dir resolve failed (%v); proceeding without trace\n", err)
		return "(none)", nil
	}
	traceDir := filepath.Join(dir, "selfheal-traces")
	if err := os.MkdirAll(traceDir, 0o700); err != nil {
		fmt.Fprintf(w, "[daemon] poll: trace mkdir failed (%v); proceeding without trace\n", err)
		return "(none)", nil
	}
	path := filepath.Join(traceDir, fmt.Sprintf("heal-%d.jsonl", time.Now().UnixNano()))
	f, err := os.Create(path)
	if err != nil {
		fmt.Fprintf(w, "[daemon] poll: trace create failed (%v); proceeding without trace\n", err)
		return "(none)", nil
	}
	return path, f
}

func init() {
	daemonCmd.Flags().IntVar(&daemonPollIntervalS, "poll-interval", 0, "seconds between /usage polls (0 = use config)")
	daemonCmd.Flags().IntVar(&daemonIngestIntervalS, "ingest-interval", 0, "seconds between JSONL ingests (0 = use config)")
	daemonCmd.Flags().IntVar(&daemonAggregateIntervalS, "aggregate-interval", 0, "seconds between aggregate refreshes (0 = use config)")
	daemonCmd.Flags().IntVar(&daemonTrailIntervalS, "trail-interval", 0, "seconds between Trail cycles (0 = use config; only runs when trail_enabled)")
	daemonCmd.Flags().BoolVar(&daemonRunOnce, "once", false, "run each job once at startup, then exit")
	daemonCmd.Flags().StringVar(&daemonLogFile, "log-file", "",
		"file to mirror daemon output to (default $XDG_STATE_HOME/bloodhound/daemon.log)")
	daemonCmd.Flags().BoolVar(&daemonNoLogFile, "no-log-file", false,
		"don't write a log file (stdout only)")
	daemonCmd.Flags().BoolVar(&daemonNoAPI, "no-api", false,
		"don't start the HTTP API server (collection only)")
	rootCmd.AddCommand(daemonCmd)
}
