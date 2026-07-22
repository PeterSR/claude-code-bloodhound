package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/PeterSR/claude-code-bloodhound/internal/aggregate"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

var (
	repairDedupeHistory bool
	repairApply         bool
	repairAll           bool
	repairWindowS       float64
	repairJSON          bool
)

var repairCmd = &cobra.Command{
	Use:   "repair",
	Short: "Fix historical data inflated by a since-fixed ingest bug",
	Long: `Bare ` + "`bloodhound repair`" + ` with no mode flag prints this help and does
nothing; --dedupe-history selects the one repair mode that exists today.

Claude Code writes one JSONL line per content block of an assistant
response (text, then each tool_use) and repeats the same usage object on
every line. The old ingester emitted a turn per line instead of a turn per
response, so one API charge used to land in the database 2 to 3 times.
That's fixed now, and ` + "`bloodhound ingest --force`" + ` re-parses any session
whose .jsonl transcript still exists, using the real message.id/requestId
to dedupe exactly.

This command is for the sessions ingest can't reach: Claude Code deletes
old transcripts, so a large share of history exists only as the turns rows
the buggy ingester already wrote, and can never be re-parsed. --dedupe-history
collapses those rows heuristically instead.

The heuristic: duplicate rows from one API response are adjacent in
turn_idx and carry an identical (model, input_tokens, output_tokens,
cache_read, cache_create_5m, cache_create_1h) tuple, with near-identical
timestamps. This collapses every maximal run of adjacent turns sharing
that tuple where each consecutive pair falls within --window-s seconds of
the next.

Measured against the sessions whose transcript still exists (so the true,
requestId-deduped turn count is knowable), comparing the heuristic's output
on their still-buggy rows to that ground truth:

  window   sessions matching exactly   net token error
  2s       29%                         +36%
  10s      70%                         +7.4%
  60s      92%                         +0.19%   (default)

A wider window catches more of a real duplicate run at the cost of also
merging turns that happen to share a tuple by coincidence; a narrower one
does the opposite. In every case the error is positive: the heuristic errs
toward keeping too many rows rather than too few.

Default scope is sessions whose .jsonl is gone, determined by resolving
each session's turns.source_path_hash to ingested_files.path and checking
with os.Stat whether that path still exists. Anything still on disk is
better fixed exactly by ` + "`ingest --force`" + `, so it's excluded by default; pass
--all to run this heuristic over every session regardless.

That "still on disk" test doesn't mean ` + "`ingest --force`" + ` will actually
reparse the file: ingest also skips anything below MinFileSize (5,000
bytes, internal/ingest.Options.MinFileSize), even under --force. A
transcript that survives only as a tiny stub passes the os.Stat check
here, so it's excluded from the default scope, but ingest won't touch it
either, so it never actually gets repaired exactly. --all is what reaches
those sessions.

Dry run (the default) reports what would change and writes nothing.
--apply takes a full database backup first, named
bloodhound.db.backup-YYYYMMDD-HHMM beside the live database (a numeric
.2, .3, ... suffix is added if that name is already taken, so running
--apply twice in the same minute gets two distinct backups instead of the
second run failing), and prints the chosen path before touching anything;
if the backup can't be written the whole repair aborts with nothing
changed. It then rewrites the affected
sessions' turns in a single transaction (turn_idx renumbered densely from
0, compactions and every other table left untouched) and re-runs
aggregation so sessions, buckets, calibration and attribution are rebuilt
from the corrected rows.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if !repairDedupeHistory {
			return cmd.Help()
		}
		if repairWindowS <= 0 {
			return fmt.Errorf("--window-s must be positive, got %v", repairWindowS)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		s, err := store.Open(ctx)
		if err != nil {
			return err
		}
		defer s.Close()

		opts := store.RepairOptions{
			WindowS: repairWindowS,
			All:     repairAll,
		}
		w := cmd.OutOrStdout()

		if !repairApply {
			stats, err := s.DedupeHistory(ctx, opts)
			if err != nil {
				return err
			}
			return printRepairStats(w, stats, false, repairJSON)
		}

		// --apply: back up before touching a single row. Any failure here
		// leaves the live database exactly as it was. NextBackupPath (not
		// BackupPath) so a second --apply within the same minute gets its
		// own numbered backup instead of failing on a name collision.
		backupPath, err := s.NextBackupPath(time.Now())
		if err != nil {
			return fmt.Errorf("backup failed, nothing changed: %w", err)
		}
		if err := s.Backup(ctx, backupPath); err != nil {
			return fmt.Errorf("backup failed, nothing changed: %w", err)
		}
		fmt.Fprintf(w, "backed up database to %s\n", backupPath)

		stats, err := s.ApplyDedupeHistory(ctx, opts)
		if err != nil {
			return err
		}
		if err := printRepairStats(w, stats, true, repairJSON); err != nil {
			return err
		}

		fmt.Fprintln(w, "re-running aggregate to rebuild sessions, buckets, calibration and attribution from the corrected turns")
		aggStats, err := aggregate.Run(ctx, s, aggregate.Options{})
		if err != nil {
			return fmt.Errorf("repair applied but aggregate failed, run `bloodhound aggregate` by hand: %w", err)
		}
		fmt.Fprintf(w, "aggregate: %d sessions, %d buckets, %d cal-points, %d attribution rows in %.2fs\n",
			aggStats.SessionsRefreshed, aggStats.BucketsRebuilt, aggStats.CalibrationPointsBuilt,
			aggStats.AttributionRowsBuilt, aggStats.ElapsedS)
		return nil
	},
}

func init() {
	repairCmd.Flags().BoolVar(&repairDedupeHistory, "dedupe-history", false,
		"collapse duplicate turns left by the pre-fix ingester (the only repair mode today)")
	repairCmd.Flags().BoolVar(&repairApply, "apply", false,
		"write the repair (default is a dry run that only reports what would change)")
	repairCmd.Flags().BoolVar(&repairAll, "all", false,
		"process every session, not just ones ingest --force can no longer reach")
	repairCmd.Flags().Float64Var(&repairWindowS, "window-s", store.DefaultRepairWindowS,
		"max gap in seconds between adjacent turns still counted as one duplicated response")
	repairCmd.Flags().BoolVar(&repairJSON, "json", false, "emit JSON stats instead of human text")
	rootCmd.AddCommand(repairCmd)
}

func printRepairStats(w io.Writer, stats store.RepairStats, applied bool, asJSON bool) error {
	if asJSON {
		out := struct {
			Applied bool `json:"applied"`
			store.RepairStats
		}{Applied: applied, RepairStats: stats}
		return writeJSONOut(w, out)
	}

	verb := "would collapse"
	if applied {
		verb = "collapsed"
	}
	fmt.Fprintf(w, "repair: %s sessions in scope, %s turns examined\n",
		fmtCount(int64(stats.SessionsInScope)), fmtCount(int64(stats.TurnsExamined)))
	fmt.Fprintf(w, "  %s %s runs across %s sessions, removing %s turns\n",
		verb, fmtCount(int64(stats.RunsCollapsed)), fmtCount(int64(stats.SessionsChanged)), fmtCount(int64(stats.TurnsRemoved)))
	fmt.Fprintf(w, "  raw tokens: %s before, %s after\n",
		fmtTokens(float64(stats.RawTokensBefore)), fmtTokens(float64(stats.RawTokensAfter)))
	if !applied && stats.TurnsRemoved > 0 {
		fmt.Fprintln(w, "  dry run: nothing written. Re-run with --apply to write these changes.")
	}
	return nil
}
