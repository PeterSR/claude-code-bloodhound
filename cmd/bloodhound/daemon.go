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

	"github.com/PeterSR/claude-code-bloodhound/internal/account"
	"github.com/PeterSR/claude-code-bloodhound/internal/aggregate"
	"sort"
	"strings"

	"github.com/PeterSR/claude-code-bloodhound/internal/announce"
	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
	"github.com/PeterSR/claude-code-bloodhound/internal/api/server"
	"github.com/PeterSR/claude-code-bloodhound/internal/config"
	"github.com/PeterSR/claude-code-bloodhound/internal/costweight"
	"github.com/PeterSR/claude-code-bloodhound/internal/costweight/priceheal"
	"github.com/PeterSR/claude-code-bloodhound/internal/events"
	"github.com/PeterSR/claude-code-bloodhound/internal/events/sensors"
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
		runReconcileOnce(ctx, s, w)
		if cfg.PriceSelfHeal {
			runPriceHealOnce(ctx, cfg, s, w)
		}
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
		// Reconcile gets its own short cadence rather than riding the poll.
		// Pending one-shots have to fire, and expiries have to be swept, even
		// while polls are failing, which is precisely when a consumer waiting
		// on collection health most needs to hear something.
		schedule("events", reconcileIvl, func() { runReconcileOnce(ctx, s, w) })
		schedule("aggregate", aggIvl, func() { runAggregateOnce(ctx, s, w) })
		schedule("event-retention", aggIvl, func() { runEventRetentionOnce(ctx, s, w) })
		// Price discovery rides the aggregate cadence. Gated per-tick on the
		// live config so toggling it in the UI takes effect without a daemon
		// restart, same as Trail.
		schedule("price-heal", aggIvl, func() {
			pc, err := config.Load()
			if err != nil {
				pc = cfg
			}
			if !pc.PriceSelfHeal {
				return
			}
			runPriceHealOnce(ctx, pc, s, w)
		})
		// Always schedule the trail ticker; gate per-tick on the LIVE
		// config so enabling/disabling Trail (or changing mode/window) in
		// the UI takes effect without a daemon restart. (Scheduling it
		// only when enabled-at-startup was a bug: toggling it on later
		// never started the job.)
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

// liveClaudeDirs resolves the config dirs to watch from the config file as it
// is now, so adding an account's dir takes effect without a daemon restart.
// Falls back to the default dir alone if the config cannot be read.
func liveClaudeDirs(w io.Writer) []string {
	cfg, err := config.Load()
	if err != nil {
		cfg = config.Default()
	}
	dirs, err := config.ClaudeConfigDirs(cfg)
	if err != nil {
		fmt.Fprintf(w, "[daemon] claude_dirs: %v\n", err)
		return nil
	}
	return dirs
}

// runPollOnce scrapes /usage once for every watched config dir, recording
// each reading against the account that dir is logged in to. Dirs with no
// subscription login have no meter and are skipped.
func runPollOnce(ctx context.Context, cfg config.Config, s *store.Store, w io.Writer) {
	for _, dir := range liveClaudeDirs(w) {
		if err := ctx.Err(); err != nil {
			return
		}
		accountID, ident, err := account.Observe(ctx, s, dir, time.Now())
		if err != nil {
			fmt.Fprintf(w, "[daemon] poll %s: account: %v\n", dir, err)
			continue
		}
		if !ident.OAuth {
			continue
		}
		pollDirOnce(ctx, cfg, s, w, dir, accountID)
	}
}

func pollDirOnce(ctx context.Context, cfg config.Config, s *store.Store, w io.Writer, dir string, accountID int64) {
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
	res, fetchErr := usage.Fetch(pollCtx, usage.Options{ClaudeBinary: cfg.ClaudeBinary, ConfigDir: dir})

	// Self-heal: if extraction missed required fields, hand the heal
	// off to selfheal.Run — which spawns its own inner claude in a pty
	// and lets an orchestrator claude -p drive it via MCP tools (read_pty,
	// send_keys, test_regex, save_extractor) until the new extractor is
	// validated and persisted. Gated by config so users who want manual
	// control (or who don't want self-heal consuming usage on their
	// account) can flip it off and use the Debug page's manual retrain.
	//
	// Gated on the panel actually having rendered. A capture that timed
	// out while /usage was still loading has no percentages on screen for
	// any extractor to find, so a heal against it burns ~30s of the user's
	// own quota to "fix" regexes that were never wrong, then saves the
	// result, replacing a working extractor with one trained on a screen
	// that never showed the panel. Those polls just retry next cycle.
	switch {
	case !cfg.ExtractorSelfHeal || fetchErr != nil || res.OK:
		// Nothing to heal, or self-heal is off.
	case !usage.PanelCaptured(res.RawFull):
		fmt.Fprintf(w, "[daemon] poll: no /usage panel in capture after %.1fs (still loading?), skipping self-heal\n", res.ElapsedS)
	default:
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

	obs, err := s.RecordUsage(pollCtx, accountID, res, fetchErr)
	switch {
	case err != nil:
		fmt.Fprintf(w, "[daemon] poll: record error %v\n", err)
	case fetchErr != nil && !errors.Is(fetchErr, context.Canceled):
		fmt.Fprintf(w, "[daemon] poll: scrape failed (%v)\n", fetchErr)
	case !res.OK && !usage.PanelCaptured(res.RawFull):
		// Distinct from the line below on purpose: "extraction failed"
		// reads as the extractor being wrong, and here it never got a
		// panel to read in the first place.
		fmt.Fprintf(w, "[daemon] poll: no usage panel captured in %.1fs, nothing to extract\n", res.ElapsedS)
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
		fmt.Fprintf(w, "[daemon] poll: ok session=%s week=%s account=%d (#%d, %.1fs)\n",
			sess, week, accountID, obs.ID, res.ElapsedS)
	}
}

// reconcileIvl is the events cadence. Fixed rather than configurable for now:
// it is pure reads plus a small transaction, so it is cheap in a way the poll
// is not, and a one-shot that has to wait five minutes to fire is a worse
// default than a tick nobody notices.
const reconcileIvl = 60 * time.Second

// eventRetention is how long the log and finished one-shots are kept. Long
// enough that a consumer down for an ordinary interval (an overnight suspend,
// a weekend) still finds what it missed, and the floor below still protects
// anything a pending one-shot has not been matched against.
const eventRetention = 14 * 24 * time.Hour

func runReconcileOnce(ctx context.Context, s *store.Store, w io.Writer) {
	if err := ctx.Err(); err != nil {
		return
	}
	// A budget of its own, like every other job. Without one this inherits the
	// daemon's root context, and a hook claimed just before SIGTERM would be
	// killed after being marked fired.
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	st, err := sensors.Run(ctx, s, time.Now())
	if err != nil {
		fmt.Fprintf(w, "[daemon] events: %v\n", err)
		return
	}
	// Stay quiet on the ordinary no-op tick, which is most of them.
	if st.Emitted > 0 || st.HooksFired > 0 || st.HooksExpired > 0 {
		fmt.Fprintf(w, "[daemon] events: emitted=%d hooks_fired=%d hooks_expired=%d\n",
			st.Emitted, st.HooksFired, st.HooksExpired)
	}
	for _, e := range st.Errors {
		fmt.Fprintf(w, "[daemon] events: warn %s\n", e)
	}

	// Delivery rides the same tick as the reconcile that produced the events,
	// so a transition is spoken about while it is still true rather than up to
	// a cadence later. It is deliberately after the reconcile and outside its
	// error path: failing to deliver must never stop the log being written,
	// because the log is the durable record and delivery is best effort.
	runAnnounceOnce(ctx, s, w)
}

// runAnnounceOnce tells whichever sessions are awake about pressure that just
// changed. Most passes say nothing, either because nothing transitioned or
// because every session was at rest, and both are the system working.
func runAnnounceOnce(ctx context.Context, s *store.Store, w io.Writer) {
	ast, err := announce.Run(ctx, s, announce.Options{})
	if err != nil {
		fmt.Fprintf(w, "[daemon] announce: %v\n", err)
		return
	}
	if ast.Delivered > 0 {
		line := fmt.Sprintf("[daemon] announce: said %d thing(s) to %d session(s)",
			ast.Announced, ast.Delivered)
		if ast.Armed > 0 {
			line += fmt.Sprintf(", noted %d down for a wakeup", ast.Armed)
		}
		fmt.Fprintln(w, line)
	}
	// A kept promise is the one delivery nobody was awake for, so it says so
	// even on a tick where nothing else happened.
	if ast.Resumed > 0 {
		fmt.Fprintf(w, "[daemon] announce: woke %d session(s) whose window reopened\n", ast.Resumed)
	}
	// Skips are only worth a line when there was something to say, otherwise
	// every quiet tick would report the whole machine sitting at its prompt.
	if ast.Announced > 0 && ast.Delivered == 0 && len(ast.Skipped) > 0 {
		fmt.Fprintf(w, "[daemon] announce: %d thing(s) to say, nobody awake to tell (%s)\n",
			ast.Announced, skipSummary(ast.Skipped))
	}
	for _, e := range ast.Errors {
		fmt.Fprintf(w, "[daemon] announce: warn %s\n", e)
	}
}

func skipSummary(skipped map[string]int) string {
	reasons := make([]string, 0, len(skipped))
	for r := range skipped {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	parts := make([]string, 0, len(reasons))
	for _, r := range reasons {
		parts = append(parts, fmt.Sprintf("%s=%d", r, skipped[r]))
	}
	return strings.Join(parts, ", ")
}

// runEventRetentionOnce trims the log. Rides the aggregate cadence rather than
// the events tick, since there is nothing to gain from running it every minute.
func runEventRetentionOnce(ctx context.Context, s *store.Store, w io.Writer) {
	if err := ctx.Err(); err != nil {
		return
	}
	now := time.Now()
	cutoff := now.Add(-eventRetention).UnixMilli()

	// Never delete below what a pending one-shot still has to be matched
	// against. Age alone would silently swallow the event a hook is waiting
	// for, which is the one outcome that makes this worse than not pruning.
	floor, err := events.OldestPendingCursor(ctx, s.DB, now)
	if err != nil {
		fmt.Fprintf(w, "[daemon] events: retention floor: %v\n", err)
		return
	}

	n, err := events.Prune(ctx, s.DB, cutoff, floor)
	if err != nil {
		fmt.Fprintf(w, "[daemon] events: prune: %v\n", err)
		return
	}
	hn, err := events.PruneHooks(ctx, s.DB, cutoff)
	if err != nil {
		fmt.Fprintf(w, "[daemon] events: prune hooks: %v\n", err)
		return
	}
	ln, err := events.PruneLevels(ctx, s.DB, cutoff)
	if err != nil {
		fmt.Fprintf(w, "[daemon] events: prune levels: %v\n", err)
		return
	}
	// Kept promises are history on the same terms as the events that caused
	// them. Pending ones are never touched however old, because a promise
	// still waiting is the one row in that table that is not a record of the
	// past; expire_ms is what ends those.
	wn, err := s.PruneWakeups(ctx, cutoff)
	if err != nil {
		fmt.Fprintf(w, "[daemon] events: prune wakeups: %v\n", err)
		return
	}
	if n > 0 || hn > 0 || ln > 0 || wn > 0 {
		fmt.Fprintf(w, "[daemon] events: pruned %d events, %d finished one-shots, %d dormant session levels, %d kept wakeups\n", n, hn, ln, wn)
	}
}

func runIngestOnce(ctx context.Context, s *store.Store, w io.Writer) {
	if err := ctx.Err(); err != nil {
		return
	}
	ictx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	stats, err := ingest.Run(ictx, s, ingest.Options{ConfigDirs: liveClaudeDirs(w)})
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

// priceHealMaxPerCycle bounds how many unknown models one cycle will try to
// price, so a burst of new model names can't tie the scheduler up in a long
// chain of web lookups. The rest wait for the next cycle; per-model backoff
// keeps failures from repeating.
const priceHealMaxPerCycle = 3

// runPriceHealOnce discovers models that appear in the logs but aren't in
// the price table and asks a headless claude to look up each one's list
// price. Best-effort and gated: a model that can't be priced stays dropped
// from the cost-weighted analysis (flagged in the UI) rather than blocking
// anything. Gated at the call site on cfg.PriceSelfHeal.
func runPriceHealOnce(ctx context.Context, cfg config.Config, s *store.Store, w io.Writer) {
	if err := ctx.Err(); err != nil {
		return
	}
	models, err := s.ModelsWithSpend(ctx)
	if err != nil {
		fmt.Fprintf(w, "[daemon] price-heal: list models: %v\n", err)
		return
	}
	unpriced := costweight.Current().UnpricedModels(models)
	if len(unpriced) == 0 {
		return
	}

	healed := 0
	for _, m := range unpriced {
		if healed >= priceHealMaxPerCycle {
			fmt.Fprintf(w, "[daemon] price-heal: %d more unpriced model(s) deferred to next cycle\n",
				len(unpriced)-healed)
			break
		}
		hctx, cancel := context.WithTimeout(ctx, 130*time.Second)
		res, gateErr := priceheal.Shared().Heal(hctx, m, priceheal.Options{
			ClaudeBinary: cfg.ClaudeBinary,
		})
		cancel()
		if gateErr != nil {
			// Cooling down or another heal running — skip quietly, not a
			// failure of this model's lookup.
			continue
		}
		healed++
		if res.Saved {
			fmt.Fprintf(w, "[daemon] price-heal: priced %s at $%g/$%g input/output (%s), $%.4f\n",
				m, res.Price.Input, res.Price.Output, res.Price.Source, res.CostUSD)
		} else {
			fmt.Fprintf(w, "[daemon] price-heal: %s left unpriced — %s\n", m, res.Reason)
		}
	}
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
