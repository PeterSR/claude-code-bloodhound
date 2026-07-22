package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
	"github.com/PeterSR/claude-code-bloodhound/internal/attribute"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

// Flags shared across the three commands. --bucket, --days, --json and
// --porcelain apply to all of them. --by, --limit, --per-window and --window
// only mean something for the rollup (the bare `attribution` command), but
// cobra has nowhere better to declare them since all three share one
// persistent flag set.
var (
	attrBucket    string
	attrDays      int
	attrJSON      bool
	attrPorcelain bool
	attrBy        string
	attrLimit     int
	attrSlices    bool
	attrPerWindow bool
	attrWindow    string
)

var attributionCmd = &cobra.Command{
	Use:   "attribution",
	Short: "Show where the /usage limit meters actually went",
	Long: `Reads the session_attribution and limit_windows tables straight out of
SQLite (no daemon socket needed, so this works with the daemon stopped) and
reports how much of a limit meter each project, session, or absolute working
directory consumed.

With no subcommand this runs the rollup: one row per project (or per session
with --by session, or per absolute working directory with --by cwd) across
the requested lookback window. project is the sanitized directory name
Claude Code invents for its transcript layout, and it is coarser than it
looks: one project can span several literal directories (the same repo
checked out twice, say), so --by cwd is the one that answers "how much did
THIS directory cost", not "how much did this project cost".

  bloodhound attribution windows        the limit windows themselves
  bloodhound attribution session <id>   one session, window by window

The rollup's own Pct sums every limit window inside the lookback, which is
the wrong number to denominate a grant against once the range spans more
than one: a 5h rollup over --days 7 sums roughly 30 windows, so its Pct
answers "how much this week", never "how much of my current 5h budget".
Two flags narrow it to the grant currency, "share of one window":

  --window current    keep only the single currently open window
  --per-window         add each group's per-window breakdown as an array

--window current's "in progress" comes from limit_windows, never inferred
from the wall clock: the 5h window is usage-triggered, not aligned to a
fixed schedule, so a caller can't derive "is this the open window" from the
current time on its own. Combine the two to see a group's share of just the
open window; use --per-window alone to see its rate across every window in
the lookback instead of only the current one.

--porcelain output is a stable parsing contract: tab separated, one record
per line, no header row. The rollup's columns, in order, are:

  key  pct  measured_pct  estimated_pct  peak_pct  share  sessions  windows
  turns  cw_tokens  raw_tokens  first_ts_unix_ms  last_ts_unix_ms  cwd

All percentages are printed at 4 decimal places (%.4f), cw_tokens and
tokens_per_pct_cw at 2 (%.2f), share as a 0 to 1 fraction at 4 decimals, and
everything else as a plain integer. An empty key (the unattributed
remainder) prints as a single "-", and so does cwd whenever it isn't a
single well-defined value: with --by project (one project can span many
directories) it is always "-"; with --by session or --by cwd it is "-" only
when the effective owner's cwd was never captured (transcript rotated off
disk before this column existed).

Under --by cwd the key column carries the directory itself rather than a
project name or session id, and a session whose cwd was never captured
doesn't vanish into, or merge with, the unattributed remainder above (key
""): it gets its own explicit bucket, key "__unknown_cwd__". The two are
different facts, not the same gap: unattributed means no turn of ours
explains the meter movement at all, while unknown cwd means we know exactly
which session(s) spent it, just not where. One sentinel standing in for
both would erase that difference from the output.

cwd is appended as the 14th column (was 13 before it was added). Deliberately
NOT added to "attribution windows" or "attribution session", whose porcelain
column counts (8 / 8 / 11) are unchanged: those rows are already keyed by a
session or project a caller can look up in the rollup for its cwd, so adding
it there too would just repeat the same value on every window line instead
of once per session.

--porcelain --per-window is a DIFFERENT record shape, not the 14 columns
above with extra fields tacked on: a per-group array doesn't fit a flat row,
so it emits one row per (group, window) instead of one row per group.
Columns, in order (10 columns):

  key  window_start_unix_ms  window_end_unix_ms  in_progress  pct
  measured_pct  estimated_pct  turns  cw_tokens  raw_tokens

key is the same value the plain rollup's key column would print for that
group (including "-" for the unattributed remainder); in_progress is "1" or
"0". Percentages print at 4 decimal places (%.4f), cw_tokens at 2 (%.2f),
everything else as a plain integer.`,
	Args: cobra.NoArgs,
	RunE: runAttributionRollup,
}

var attributionWindowsCmd = &cobra.Command{
	Use:   "windows",
	Short: "List the limit windows for a bucket",
	Long: `Lists the reconstructed limit windows (weekly or 5 hour) for the
requested lookback, one line per window. --slices expands each window into
one line per effective owner that filled it: a subagent (Task tool) session
folds into whichever session dispatched it, the same rollup
"attribution --by session" already applies, so the two commands agree on
where a window's spend went. A subagent's own detail is still reachable
through "bloodhound attribution session <uuid>".

--porcelain columns, in order, without --slices (8 columns):

  start_unix_ms  end_unix_ms  reset_unix_ms  flags  measured_pct
  attributed_pct  peak_pct  tokens_per_pct_cw

--porcelain columns, in order, with --slices (8 columns):

  window_start_unix_ms  key  pct  measured_pct  estimated_pct  turns
  cw_tokens  raw_tokens

Percentages print at 4 decimal places (%.4f), cw_tokens and
tokens_per_pct_cw at 2 (%.2f), everything else as a plain integer. flags is
"-" when nothing applies, otherwise a comma joined list drawn from
inferred, partial, in_progress, hit_cap. key is the effective owner (see
above), or "-" for the unattributed remainder.`,
	Args: cobra.NoArgs,
	RunE: runAttributionWindows,
}

var attributionSessionCmd = &cobra.Command{
	Use:   "session [id]",
	Short: "Show one session's cost against both limit meters, window by window",
	Long: `Resolves <id> as a full session UUID or a unique prefix of one (like a
git short SHA) and walks its cost against both the weekly and the 5 hour
meter, one line per limit window it touched.

<id> can be omitted: it then defaults to "bloodhound session" (the current
Claude Code session, read from CLAUDE_CODE_SESSION_ID), so this only works
run from inside a session Claude Code itself spawned. Outside one, omitting
<id> fails with the same message "bloodhound session" gives on its own.

Human and JSON output always show both buckets. --bucket only narrows
--porcelain: pass it explicitly to emit just that bucket's rows, or leave
it unset (the default) to get both.

--porcelain columns, in order (11 columns):

  bucket  window_start_unix_ms  window_end_unix_ms  flags  pct
  measured_pct  estimated_pct  window_pct  turns  cw_tokens  raw_tokens

Percentages print at 4 decimal places (%.4f), cw_tokens at 2 (%.2f),
everything else as a plain integer. flags is "-" when nothing applies,
otherwise a comma joined list drawn from inferred, partial, in_progress,
hit_cap.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runAttributionSession,
}

func init() {
	attributionCmd.PersistentFlags().StringVar(&attrBucket, "bucket", attribute.BucketWeek,
		`limit meter to report on: "week" or "5h"`)
	attributionCmd.PersistentFlags().IntVar(&attrDays, "days", 0,
		"lookback in days (0 uses the bucket's default: 56 for week, 7 for 5h)")
	attributionCmd.PersistentFlags().BoolVar(&attrJSON, "json", false, "emit JSON")
	attributionCmd.PersistentFlags().BoolVar(&attrPorcelain, "porcelain", false,
		"emit stable, tab separated records for scripting")
	attributionCmd.PersistentFlags().StringVar(&attrBy, "by", "project",
		`rollup grouping, rollup only: "project", "session", or "cwd"`)
	attributionCmd.PersistentFlags().IntVar(&attrLimit, "limit", 20,
		"max rollup rows in human mode, 0 for all (rollup only, ignored by --json/--porcelain)")
	attributionCmd.PersistentFlags().BoolVar(&attrPerWindow, "per-window", false,
		"rollup only: add each group's per-window breakdown, its share of one window at a time")
	attributionCmd.PersistentFlags().StringVar(&attrWindow, "window", "",
		`rollup only: "current" keeps only the single currently open window`)

	attributionWindowsCmd.Flags().BoolVar(&attrSlices, "slices", false,
		"expand to one record per (window, slice) instead of one per window")

	attributionCmd.AddCommand(attributionWindowsCmd)
	attributionCmd.AddCommand(attributionSessionCmd)
	rootCmd.AddCommand(attributionCmd)
}

// ---------------------------------------------------------------------
// flag validation, shared by all three commands
// ---------------------------------------------------------------------

func attrValidateFormatFlags() error {
	if attrJSON && attrPorcelain {
		return fmt.Errorf("--json and --porcelain cannot both be set")
	}
	return nil
}

func attrResolveBucket() (string, error) {
	switch attrBucket {
	case attribute.BucketWeek, attribute.Bucket5h:
		return attrBucket, nil
	default:
		return "", fmt.Errorf(`invalid --bucket %q: must be "week" or "5h"`, attrBucket)
	}
}

func attrResolveBy() (string, error) {
	switch attrBy {
	case "project", "session", "cwd":
		return attrBy, nil
	default:
		return "", fmt.Errorf(`invalid --by %q: must be "project", "session", or "cwd"`, attrBy)
	}
}

// attrResolveWindow validates --window, rollup only. "" means no filter (the
// ordinary --days lookback); "current" is the only other value understood
// today, so anything else is rejected rather than silently ignored, unlike
// the API's equally permissive but error-free query param of the same name
// (a CLI typo deserves a message; a stray query string does not).
func attrResolveWindow() (string, error) {
	switch attrWindow {
	case "", "current":
		return attrWindow, nil
	default:
		return "", fmt.Errorf(`invalid --window %q: must be "current" (or omit it for the full --days lookback)`, attrWindow)
	}
}

// attrDaysOrDefault validates --days and, when it's the 0 sentinel,
// resolves it to the bucket's default lookback.
func attrDaysOrDefault(bucket string) (int, error) {
	if attrDays < 0 {
		return 0, fmt.Errorf("--days must not be negative")
	}
	if attrDays > 0 {
		return attrDays, nil
	}
	if bucket == attribute.Bucket5h {
		return 7, nil
	}
	return 56, nil
}

// ---------------------------------------------------------------------
// rollup: bloodhound attribution
// ---------------------------------------------------------------------

func runAttributionRollup(cmd *cobra.Command, args []string) error {
	if err := attrValidateFormatFlags(); err != nil {
		return err
	}
	bucket, err := attrResolveBucket()
	if err != nil {
		return err
	}
	by, err := attrResolveBy()
	if err != nil {
		return err
	}
	days, err := attrDaysOrDefault(bucket)
	if err != nil {
		return err
	}
	windowFilter, err := attrResolveWindow()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := store.Open(ctx)
	if err != nil {
		return err
	}
	defer s.Close()

	since := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()

	windows, err := s.ListLimitWindows(ctx, bucket, since)
	if err != nil {
		return err
	}

	// --window current narrows everything below to the single open window.
	// Found from the --days-scoped list just fetched when that already
	// reaches far enough back; --days describes how far the ordinary
	// (unfiltered) rollup looks, not a bound on how old the open window can
	// be, so a narrow --days (--days 7 on a week bucket, say) could
	// otherwise miss a window that's genuinely still open just because its
	// start predates the cutoff. attrCurrentWindowSince is independent of
	// --days for exactly that reason.
	windowOpen := true
	if windowFilter == "current" {
		curWindows := windows
		if curSince := attrCurrentWindowSince(bucket); curSince < since {
			if curWindows, err = s.ListLimitWindows(ctx, bucket, curSince); err != nil {
				return err
			}
		}
		if cur := attrCurrentWindow(curWindows); cur != nil {
			// A window's own start can never be in the future, so using it
			// as the new "since" is exactly "only this window": nothing
			// newer exists to leak back in.
			since = cur.StartUnixMS
			windows = []store.LimitWindowRow{*cur}
		} else {
			windowOpen = false
			windows = []store.LimitWindowRow{}
		}
	}

	var groups []store.AttributionGroup
	var slices []store.AttributionRow
	if windowOpen {
		if groups, err = s.GroupAttribution(ctx, bucket, by, since); err != nil {
			return err
		}
		if attrPerWindow {
			if slices, err = s.WindowSlices(ctx, bucket, since); err != nil {
				return err
			}
		}
	}

	var totalPct, measuredPct, estimatedPct, unattributedPct float64
	for _, g := range groups {
		totalPct += g.Pct
		measuredPct += g.MeasuredPct
		estimatedPct += g.EstimatedPct
		if g.Key == attribute.Unattributed {
			unattributedPct += g.Pct
		}
	}

	var tokensPerPctCW float64
	for i := len(windows) - 1; i >= 0; i-- {
		if windows[i].TokensPerPctCW > 0 {
			tokensPerPctCW = windows[i].TokensPerPctCW
			break
		}
	}

	var perWindow map[string][]routes.AttrGroupWindow
	if attrPerWindow {
		perWindow = buildAttrGroupWindows(windows, slices, by)
	}

	w := cmd.OutOrStdout()
	switch {
	case attrJSON:
		return writeAttrRollupJSON(w, bucket, by, days, windowFilter, tokensPerPctCW,
			totalPct, measuredPct, estimatedPct, unattributedPct, groups, perWindow)
	case attrPorcelain:
		if attrPerWindow {
			return writeAttrRollupPorcelainPerWindow(w, groups, perWindow)
		}
		return writeAttrRollupPorcelain(w, groups, totalPct)
	default:
		return writeAttrRollupHuman(w, bucket, by, days, len(windows), tokensPerPctCW,
			totalPct, measuredPct, estimatedPct, unattributedPct, groups, attrLimit,
			windowFilter, windowOpen, perWindow)
	}
}

// attrCurrentWindowSince is how far back "--window current" is guaranteed
// to search for the open window, regardless of --days: doubled past each
// bucket's real span as slack for a window that ran unusually long or a
// late aggregator pass, so the search only ever widens past whatever --days
// already covers, never narrows it.
func attrCurrentWindowSince(bucket string) int64 {
	if bucket == attribute.Bucket5h {
		return time.Now().Add(-10 * time.Hour).UnixMilli()
	}
	return time.Now().Add(-14 * 24 * time.Hour).UnixMilli()
}

// attrCurrentWindow returns the one in-progress window in a bucket's list,
// or nil when none is open. Walked from the end because ListLimitWindows
// returns oldest first, so the open window - if there is one - is always
// the last entry.
func attrCurrentWindow(windows []store.LimitWindowRow) *store.LimitWindowRow {
	for i := len(windows) - 1; i >= 0; i-- {
		if windows[i].InProgress {
			return &windows[i]
		}
	}
	return nil
}

// rollupGroupKey derives the grouping key one WindowSlices row contributes
// under a "by" mode, matching GroupAttribution's own SQL exactly (mirrored
// here rather than imported: internal/api/server's version is unexported,
// same reason the store-row conversions at the bottom of this file are
// duplicated rather than shared, see that section's comment).
func rollupGroupKey(r store.AttributionRow, by string) string {
	key := r.EffectiveSessionUUID
	if by == "project" {
		key = r.Project
	}
	if by == "cwd" {
		key = r.Cwd
	}
	switch {
	case r.SessionUUID == attribute.Unattributed:
		key = attribute.Unattributed
	case by == "cwd" && r.Cwd == "":
		key = store.UnknownCwd
	}
	return key
}

// buildAttrGroupWindows groups WindowSlices rows by (group key, window),
// the transpose of the per-window slices GroupAttribution and the rollup
// otherwise show: one array per GROUP instead of one array per window. This
// is what --per-window adds, and it's cheap because windows/slices are
// already fetched for the ordinary rollup call.
func buildAttrGroupWindows(windows []store.LimitWindowRow, slices []store.AttributionRow, by string) map[string][]routes.AttrGroupWindow {
	type agg struct {
		measured, estimated, cw float64
		raw                     int64
		turns                   int
	}
	byWindow := map[int64]map[string]*agg{}
	for _, r := range slices {
		key := rollupGroupKey(r, by)
		m, ok := byWindow[r.WindowStartUnixMS]
		if !ok {
			m = map[string]*agg{}
			byWindow[r.WindowStartUnixMS] = m
		}
		a, ok := m[key]
		if !ok {
			a = &agg{}
			m[key] = a
		}
		a.measured += r.MeasuredPct
		a.estimated += r.EstimatedPct
		a.cw += r.CWTokens
		a.raw += r.RawTokens
		a.turns += r.TurnCount
	}

	// windows is oldest first (ListLimitWindows' own contract), so ranging
	// over it in order is what keeps every group's per-window array
	// ascending too, without a second sort pass.
	out := map[string][]routes.AttrGroupWindow{}
	for _, win := range windows {
		for key, a := range byWindow[win.StartUnixMS] {
			out[key] = append(out[key], routes.AttrGroupWindow{
				WindowStartUnixMS: win.StartUnixMS,
				WindowEndUnixMS:   win.EndUnixMS,
				InProgress:        win.InProgress,
				Pct:               round2(a.measured + a.estimated),
				MeasuredPct:       round2(a.measured),
				EstimatedPct:      round2(a.estimated),
				CWTokens:          round2(a.cw),
				RawTokens:         a.raw,
				TurnCount:         a.turns,
			})
		}
	}
	return out
}

func writeAttrRollupHuman(w io.Writer, bucket, by string, days, windowCount int, tokensPerPctCW float64,
	totalPct, measuredPct, estimatedPct, unattributedPct float64, groups []store.AttributionGroup, limit int,
	windowFilter string, windowOpen bool, perWindow map[string][]routes.AttrGroupWindow) error {
	if len(groups) == 0 {
		if windowFilter == "current" && !windowOpen {
			fmt.Fprintf(w, "no %s window is currently open.\n", bucketLabel(bucket))
			return nil
		}
		fmt.Fprintf(w, "nothing attributed yet for the %s bucket in the last %d days. Run `bloodhound aggregate` to build it.\n", bucket, days)
		return nil
	}

	if windowFilter == "current" {
		fmt.Fprintf(w, "%s, by %s, current window only\n", bucketLabel(bucket), byLabel(by))
	} else {
		fmt.Fprintf(w, "%s, by %s, last %d days across %d windows\n", bucketLabel(bucket), byLabel(by), days, windowCount)
	}
	fmt.Fprintf(w, "  %s attributed, %s measured, %s estimated, %s unattributed\n",
		fmtPct(totalPct), fmtPct(measuredPct), fmtPct(estimatedPct), fmtPct(unattributedPct))
	if tokensPerPctCW > 0 {
		fmt.Fprintf(w, "  1 point costs about %s weighted tokens\n", fmtTokens(tokensPerPctCW))
	}
	fmt.Fprintln(w)

	rows := groups
	if limit > 0 && limit < len(rows) {
		rows = rows[:limit]
	}

	lastHeader := "PROJECT"
	switch by {
	case "session":
		lastHeader = "SESSION"
	case "cwd":
		lastHeader = "WORKING DIR"
	}

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "PCT\tSHARE\tPEAK\tSESSIONS\tTURNS\t%s\n", lastHeader)
	for _, g := range rows {
		share := 0.0
		if totalPct > 0 {
			share = g.Pct / totalPct * 100
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			fmtPct(g.Pct), fmtPct(share), fmtPct(g.PeakPct),
			fmtCount(int64(g.Sessions)), fmtCount(int64(g.TurnCount)),
			rollupLastColumn(g, by))
		if perWindow != nil {
			// Flush around the per-window detail lines the same way
			// "attribution windows --slices" does: their key column can be a
			// full UUID, much wider than this table's own columns, and
			// interleaving them in the same tabwriter would stretch every
			// other group row to match.
			if err := tw.Flush(); err != nil {
				return err
			}
			for _, pw := range perWindow[g.Key] {
				mark := ""
				if pw.InProgress {
					mark = " (in progress)"
				}
				fmt.Fprintf(tw, "    %s\t%s%s\n", formatLocalTime(pw.WindowStartUnixMS), fmtPct(pw.Pct), mark)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
		}
	}
	return tw.Flush()
}

func rollupLastColumn(g store.AttributionGroup, by string) string {
	if g.Key == attribute.Unattributed {
		return "(unattributed)"
	}
	if by == "session" {
		first8 := g.Key
		if len(first8) > 8 {
			first8 = first8[:8]
		}
		return first8 + "  " + g.Project
	}
	if by == "cwd" && g.Key == store.UnknownCwd {
		return "(unknown cwd)"
	}
	return g.Key
}

func writeAttrRollupJSON(w io.Writer, bucket, by string, days int, windowFilter string, tokensPerPctCW,
	totalPct, measuredPct, estimatedPct, unattributedPct float64, groups []store.AttributionGroup,
	perWindow map[string][]routes.AttrGroupWindow) error {
	out := struct {
		Bucket          string             `json:"bucket"`
		By              string             `json:"by"`
		Days            int                `json:"days"`
		Window          string             `json:"window,omitempty"`
		TokensPerPctCW  float64            `json:"tokens_per_pct_cw"`
		TotalPct        float64            `json:"total_pct"`
		MeasuredPct     float64            `json:"measured_pct"`
		EstimatedPct    float64            `json:"estimated_pct"`
		UnattributedPct float64            `json:"unattributed_pct"`
		Groups          []routes.AttrGroup `json:"groups"`
	}{
		Bucket:          bucket,
		By:              by,
		Days:            days,
		Window:          windowFilter,
		TokensPerPctCW:  round2(tokensPerPctCW),
		TotalPct:        round2(totalPct),
		MeasuredPct:     round2(measuredPct),
		EstimatedPct:    round2(estimatedPct),
		UnattributedPct: round2(unattributedPct),
		Groups:          []routes.AttrGroup{},
	}
	for _, g := range groups {
		row := attrGroupFromStore(g, totalPct)
		if perWindow != nil {
			row.PerWindow = perWindow[g.Key]
		}
		out.Groups = append(out.Groups, row)
	}
	return writeJSONOut(w, out)
}

func writeAttrRollupPorcelain(w io.Writer, groups []store.AttributionGroup, totalPct float64) error {
	for _, g := range groups {
		key := g.Key
		if key == "" {
			key = "-"
		}
		// cwd is "-" both for the unattributed sentinel and for a
		// project-keyed group (see AttributionGroup.Cwd: never a single
		// value there by design), same placeholder convention as key.
		cwd := g.Cwd
		if cwd == "" {
			cwd = "-"
		}
		share := 0.0
		if totalPct > 0 {
			share = g.Pct / totalPct
		}
		fmt.Fprintf(w, "%s\t%.4f\t%.4f\t%.4f\t%.4f\t%.4f\t%d\t%d\t%d\t%.2f\t%d\t%d\t%d\t%s\n",
			key, g.Pct, g.MeasuredPct, g.EstimatedPct, g.PeakPct, share,
			g.Sessions, g.Windows, g.TurnCount, g.CWTokens, g.RawTokens,
			g.FirstTSUnixMS, g.LastTSUnixMS, cwd)
	}
	return nil
}

// writeAttrRollupPorcelainPerWindow is --porcelain --per-window's own record
// form, not the 14-column rollup form above with fields appended: a
// per-group array of windows doesn't fit a flat row, so this emits one row
// per (group, window) instead of one row per group. 10 columns, in order:
// key, window_start_unix_ms, window_end_unix_ms, in_progress, pct,
// measured_pct, estimated_pct, turns, cw_tokens, raw_tokens.
func writeAttrRollupPorcelainPerWindow(w io.Writer, groups []store.AttributionGroup, perWindow map[string][]routes.AttrGroupWindow) error {
	for _, g := range groups {
		key := g.Key
		if key == "" {
			key = "-"
		}
		for _, pw := range perWindow[g.Key] {
			inProgress := 0
			if pw.InProgress {
				inProgress = 1
			}
			fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%.4f\t%.4f\t%.4f\t%d\t%.2f\t%d\n",
				key, pw.WindowStartUnixMS, pw.WindowEndUnixMS, inProgress,
				pw.Pct, pw.MeasuredPct, pw.EstimatedPct, pw.TurnCount, pw.CWTokens, pw.RawTokens)
		}
	}
	return nil
}

// ---------------------------------------------------------------------
// windows: bloodhound attribution windows
// ---------------------------------------------------------------------

func runAttributionWindows(cmd *cobra.Command, args []string) error {
	if err := attrValidateFormatFlags(); err != nil {
		return err
	}
	bucket, err := attrResolveBucket()
	if err != nil {
		return err
	}
	days, err := attrDaysOrDefault(bucket)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := store.Open(ctx)
	if err != nil {
		return err
	}
	defer s.Close()

	since := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()

	windows, err := s.ListLimitWindows(ctx, bucket, since)
	if err != nil {
		return err
	}

	var bySlices map[int64][]store.AttributionRow
	if attrSlices {
		rows, err := s.WindowSlices(ctx, bucket, since)
		if err != nil {
			return err
		}
		bySlices = make(map[int64][]store.AttributionRow, len(windows))
		for _, r := range rows {
			bySlices[r.WindowStartUnixMS] = append(bySlices[r.WindowStartUnixMS], r)
		}
		// Fold each window's rows onto their effective owner before any
		// writer sees them, human, --json and --porcelain alike share this
		// map. Without it a subagent would print as its own slice here
		// while "attribution --by session" already folds that same spend
		// into its parent, and the two commands would disagree on where a
		// window's spend went.
		for start, rs := range bySlices {
			bySlices[start] = foldSlicesByEffectiveOwner(rs)
		}
	}

	w := cmd.OutOrStdout()
	switch {
	case attrJSON:
		return writeAttrWindowsJSON(w, bucket, days, windows, bySlices, attrSlices)
	case attrPorcelain:
		return writeAttrWindowsPorcelain(w, windows, bySlices, attrSlices)
	default:
		return writeAttrWindowsHuman(w, bucket, days, windows, bySlices, attrSlices)
	}
}

// foldSlicesByEffectiveOwner collapses a window's rows onto EffectiveSessionUUID,
// summing the percentage, token and turn figures for any rows that share
// one: a subagent (Task tool) session and whichever session dispatched it.
// This is the same fold GroupAttribution and the web stacked chart already
// apply (see WindowSlices's doc comment for where EffectiveSessionUUID
// comes from); a subagent's own row is still reachable, unfolded, through
// `bloodhound attribution session <uuid>`, so nothing here is a loss, only
// a different key to look it up by.
func foldSlicesByEffectiveOwner(rows []store.AttributionRow) []store.AttributionRow {
	order := make([]string, 0, len(rows))
	byKey := map[string]*store.AttributionRow{}
	for _, r := range rows {
		key := r.EffectiveSessionUUID
		agg, ok := byKey[key]
		if !ok {
			cp := r
			cp.SessionUUID = key
			byKey[key] = &cp
			order = append(order, key)
			continue
		}
		agg.MeasuredPct += r.MeasuredPct
		agg.EstimatedPct += r.EstimatedPct
		agg.CWTokens += r.CWTokens
		agg.RawTokens += r.RawTokens
		agg.TurnCount += r.TurnCount
		if agg.FirstTSUnixMS == 0 || (r.FirstTSUnixMS != 0 && r.FirstTSUnixMS < agg.FirstTSUnixMS) {
			agg.FirstTSUnixMS = r.FirstTSUnixMS
		}
		if r.LastTSUnixMS > agg.LastTSUnixMS {
			agg.LastTSUnixMS = r.LastTSUnixMS
		}
	}

	out := make([]store.AttributionRow, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	// WindowSlices documents largest-share-first within a window; folding
	// can reorder rows relative to that (a parent with a small direct share
	// but large subagent spend now outranks what came before it), so
	// re-sort rather than leave the pre-fold order in place.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].MeasuredPct+out[i].EstimatedPct > out[j].MeasuredPct+out[j].EstimatedPct
	})
	return out
}

func writeAttrWindowsHuman(w io.Writer, bucket string, days int, windows []store.LimitWindowRow,
	bySlices map[int64][]store.AttributionRow, showSlices bool) error {
	if len(windows) == 0 {
		fmt.Fprintf(w, "no limit windows recorded yet for the %s bucket in the last %d days. Run `bloodhound aggregate` to build it.\n", bucket, days)
		return nil
	}

	fmt.Fprintf(w, "%s, last %d days, %d windows\n\n", bucketLabel(bucket), days, len(windows))

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "START\tEND\tATTRIBUTED\tMEASURED\tPEAK\tTOKENS/PT\tFLAGS")
	for _, win := range windows {
		tok := "-"
		if win.TokensPerPctCW > 0 {
			tok = fmtTokens(win.TokensPerPctCW)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d%%\t%s\t%s\n",
			formatLocalTime(win.StartUnixMS), formatLocalTime(win.EndUnixMS),
			fmtPct(win.AttributedPct), fmtPct(win.MeasuredPct), win.PeakPct, tok,
			humanWindowFlags(win))
		if showSlices && len(bySlices[win.StartUnixMS]) > 0 {
			// Flush around the slice detail lines so their much wider first
			// cell (a full session UUID) doesn't stretch the START column
			// of every other window row sharing this tabwriter.
			if err := tw.Flush(); err != nil {
				return err
			}
			for _, r := range bySlices[win.StartUnixMS] {
				fmt.Fprintf(tw, "    %s\t%s\t%s turns\n",
					humanKey(r.SessionUUID), fmtPct(r.MeasuredPct+r.EstimatedPct), fmtCount(int64(r.TurnCount)))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
		}
	}
	return tw.Flush()
}

func writeAttrWindowsJSON(w io.Writer, bucket string, days int, windows []store.LimitWindowRow,
	bySlices map[int64][]store.AttributionRow, showSlices bool) error {
	out := struct {
		Bucket  string              `json:"bucket"`
		Days    int                 `json:"days"`
		Windows []routes.AttrWindow `json:"windows"`
	}{Bucket: bucket, Days: days, Windows: []routes.AttrWindow{}}

	for _, win := range windows {
		aw := attrWindowFromStore(win)
		if showSlices {
			for _, r := range bySlices[win.StartUnixMS] {
				aw.Slices = append(aw.Slices, attrSliceFromStore(r))
			}
		}
		out.Windows = append(out.Windows, aw)
	}
	return writeJSONOut(w, out)
}

func writeAttrWindowsPorcelain(w io.Writer, windows []store.LimitWindowRow,
	bySlices map[int64][]store.AttributionRow, showSlices bool) error {
	for _, win := range windows {
		if showSlices {
			for _, r := range bySlices[win.StartUnixMS] {
				key := r.SessionUUID
				if key == "" {
					key = "-"
				}
				fmt.Fprintf(w, "%d\t%s\t%.4f\t%.4f\t%.4f\t%d\t%.2f\t%d\n",
					win.StartUnixMS, key, r.MeasuredPct+r.EstimatedPct, r.MeasuredPct, r.EstimatedPct,
					r.TurnCount, r.CWTokens, r.RawTokens)
			}
			continue
		}
		fmt.Fprintf(w, "%d\t%d\t%d\t%s\t%.4f\t%.4f\t%.4f\t%.2f\n",
			win.StartUnixMS, win.EndUnixMS, win.ResetUnixMS, porcelainWindowFlags(win),
			win.MeasuredPct, win.AttributedPct, float64(win.PeakPct), win.TokensPerPctCW)
	}
	return nil
}

// ---------------------------------------------------------------------
// session: bloodhound attribution session <id>
// ---------------------------------------------------------------------

func runAttributionSession(cmd *cobra.Command, args []string) error {
	if err := attrValidateFormatFlags(); err != nil {
		return err
	}
	bucket, err := attrResolveBucket()
	if err != nil {
		return err
	}
	// --days is inert for `session` (SessionPctWindows has no since param)
	// but still validated, since the flag is documented as universal.
	if _, err := attrDaysOrDefault(bucket); err != nil {
		return err
	}

	// No <id>: default to the session bloodhound itself is running inside,
	// the same lookup "bloodhound session" does. Errors with the identical
	// message when that variable isn't set, since the reason is the same
	// one: this isn't running as a child of Claude Code.
	id := ""
	if len(args) == 1 {
		id = args[0]
	} else {
		uuid, ok := currentSessionUUID()
		if !ok {
			return fmt.Errorf(
				"no <id> given and not running inside a Claude Code session: %s is not set. "+
					"Pass a session id explicitly, or run this from a process Claude Code itself spawned",
				claudeCodeSessionEnvVar)
		}
		id = uuid
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := store.Open(ctx)
	if err != nil {
		return err
	}
	defer s.Close()

	uuid, err := s.ResolveSessionUUID(ctx, id)
	if err != nil {
		return err
	}

	weekRows, weekWindows, err := s.SessionPctWindows(ctx, uuid, attribute.BucketWeek)
	if err != nil {
		return err
	}
	fiveHRows, fiveHWindows, err := s.SessionPctWindows(ctx, uuid, attribute.Bucket5h)
	if err != nil {
		return err
	}
	totalsAll, err := s.SessionPctTotalsAll(ctx)
	if err != nil {
		return err
	}
	totals := totalsAll[uuid]

	w := cmd.OutOrStdout()
	switch {
	case attrJSON:
		return writeAttrSessionJSON(w, uuid, totals, weekRows, weekWindows, fiveHRows, fiveHWindows)
	case attrPorcelain:
		return writeAttrSessionPorcelain(w, bucket, cmd.Flags().Changed("bucket"), weekRows, weekWindows, fiveHRows, fiveHWindows)
	default:
		return writeAttrSessionHuman(w, uuid, totals, weekRows, weekWindows, fiveHRows, fiveHWindows)
	}
}

func writeAttrSessionHuman(w io.Writer, uuid string, totals *store.SessionPctTotals,
	weekRows []store.AttributionRow, weekWindows []store.LimitWindowRow,
	fiveHRows []store.AttributionRow, fiveHWindows []store.LimitWindowRow) error {
	if totals == nil && len(weekRows) == 0 && len(fiveHRows) == 0 {
		fmt.Fprintf(w, "no attribution recorded yet for session %s. Run `bloodhound aggregate` to build it.\n", uuid)
		return nil
	}

	fmt.Fprintf(w, "session %s\n", uuid)
	project, cwd := "", ""
	if totals != nil {
		project = totals.Project
		cwd = totals.Cwd
	}
	fmt.Fprintf(w, "  %s\n", project)
	// "(unknown)" rather than an empty line: most often the session's
	// transcript rotated off disk before cwd existed as a column, so there
	// is nothing left to recover it from (see SessionPctTotals.Cwd). That's
	// expected for a lot of history, not a sign something broke.
	if cwd == "" {
		cwd = "(unknown)"
	}
	fmt.Fprintf(w, "  %s\n", cwd)
	if totals != nil {
		fmt.Fprintf(w, "  %s of the weekly limit across %s windows\n",
			fmtPct(totals.WeekPct), fmtCount(int64(totals.WindowWeek)))
		fmt.Fprintf(w, "  %s of the 5 hour limit across %s windows, peak %s\n",
			fmtPct(totals.FiveHPct), fmtCount(int64(totals.Windows5h)), fmtPct(totals.FiveHPeakPct))
	}

	if len(weekRows) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "weekly windows")
		writeSessionWindowTable(w, weekRows, weekWindows)
	}
	if len(fiveHRows) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "5 hour windows")
		writeSessionWindowTable(w, fiveHRows, fiveHWindows)
	}
	return nil
}

func writeSessionWindowTable(w io.Writer, rows []store.AttributionRow, wins []store.LimitWindowRow) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "START\tPCT\tOF WINDOW\tTURNS\tFLAGS")
	for i, r := range rows {
		win := wins[i]
		pct := r.MeasuredPct + r.EstimatedPct
		ofWindow := 0.0
		if win.AttributedPct > 0 {
			ofWindow = pct / win.AttributedPct * 100
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			formatLocalTime(r.WindowStartUnixMS), fmtPct(pct), fmtPct(ofWindow),
			fmtCount(int64(r.TurnCount)), humanWindowFlags(win))
	}
	tw.Flush()
}

func writeAttrSessionJSON(w io.Writer, uuid string, totals *store.SessionPctTotals,
	weekRows []store.AttributionRow, weekWindows []store.LimitWindowRow,
	fiveHRows []store.AttributionRow, fiveHWindows []store.LimitWindowRow) error {
	project, cwd := "", ""
	if totals != nil {
		project = totals.Project
		cwd = totals.Cwd
	}
	out := struct {
		SessionUUID string `json:"session_uuid"`
		Project     string `json:"project"`
		// Cwd sits alongside Project at the top level, the same place
		// Project already lives, rather than only inside Totals: this
		// envelope is CLI-bespoke (unlike Totals, which is the shared
		// routes.SessionAttribution type also used by /api/sessions), and a
		// reader shouldn't have to know that identity fields live in two
		// different places in the same JSON blob depending on which one.
		// Totals.Cwd carries the identical value; that's an intentional,
		// harmless duplication for consumers that only look at Totals.
		Cwd    string                      `json:"cwd,omitempty"`
		Totals routes.SessionAttribution   `json:"totals"`
		Week   []routes.SessionWindowSlice `json:"week"`
		FiveH  []routes.SessionWindowSlice `json:"five_h"`
	}{
		SessionUUID: uuid,
		Project:     project,
		Cwd:         cwd,
		Totals:      sessionAttributionFromStore(totals),
		Week:        []routes.SessionWindowSlice{},
		FiveH:       []routes.SessionWindowSlice{},
	}
	for i, r := range weekRows {
		out.Week = append(out.Week, sessionWindowSliceFromStore(r, weekWindows[i]))
	}
	for i, r := range fiveHRows {
		out.FiveH = append(out.FiveH, sessionWindowSliceFromStore(r, fiveHWindows[i]))
	}
	return writeJSONOut(w, out)
}

func writeAttrSessionPorcelain(w io.Writer, bucket string, bucketSpecified bool,
	weekRows []store.AttributionRow, weekWindows []store.LimitWindowRow,
	fiveHRows []store.AttributionRow, fiveHWindows []store.LimitWindowRow) error {
	emit := func(bkt string, rows []store.AttributionRow, wins []store.LimitWindowRow) {
		for i, r := range rows {
			win := wins[i]
			pct := r.MeasuredPct + r.EstimatedPct
			ofWindow := 0.0
			if win.AttributedPct > 0 {
				ofWindow = pct / win.AttributedPct * 100
			}
			fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%.4f\t%.4f\t%.4f\t%.4f\t%d\t%.2f\t%d\n",
				bkt, r.WindowStartUnixMS, win.EndUnixMS, porcelainWindowFlags(win),
				pct, r.MeasuredPct, r.EstimatedPct, ofWindow, r.TurnCount, r.CWTokens, r.RawTokens)
		}
	}

	if bucketSpecified {
		if bucket == attribute.Bucket5h {
			emit(attribute.Bucket5h, fiveHRows, fiveHWindows)
		} else {
			emit(attribute.BucketWeek, weekRows, weekWindows)
		}
		return nil
	}
	emit(attribute.BucketWeek, weekRows, weekWindows)
	emit(attribute.Bucket5h, fiveHRows, fiveHWindows)
	return nil
}

// ---------------------------------------------------------------------
// shared human formatting helpers
// ---------------------------------------------------------------------

func bucketLabel(bucket string) string {
	if bucket == attribute.Bucket5h {
		return "5 hour limit"
	}
	return "weekly limit"
}

func byLabel(by string) string {
	switch by {
	case "session":
		return "session"
	case "cwd":
		return "working directory"
	default:
		return "project"
	}
}

func humanKey(key string) string {
	if key == attribute.Unattributed {
		return "(unattributed)"
	}
	return key
}

func humanWindowFlags(win store.LimitWindowRow) string {
	var flags []string
	if win.Inferred {
		flags = append(flags, "inferred")
	}
	if win.Partial {
		flags = append(flags, "partial")
	}
	if win.InProgress {
		flags = append(flags, "in progress")
	}
	if win.HitCap {
		flags = append(flags, "hit cap")
	}
	return strings.Join(flags, ", ")
}

func porcelainWindowFlags(win store.LimitWindowRow) string {
	var flags []string
	if win.Inferred {
		flags = append(flags, "inferred")
	}
	if win.Partial {
		flags = append(flags, "partial")
	}
	if win.InProgress {
		flags = append(flags, "in_progress")
	}
	if win.HitCap {
		flags = append(flags, "hit_cap")
	}
	if len(flags) == 0 {
		return "-"
	}
	return strings.Join(flags, ",")
}

func formatLocalTime(ms int64) string {
	return time.UnixMilli(ms).Local().Format("2006-01-02 15:04")
}

// fmtPct formats a percentage for human display: always one decimal place,
// "0%" for an exact zero, and "<0.1%" for a nonzero value under 0.1. Used
// everywhere a percentage reaches a human eye.
func fmtPct(v float64) string {
	if v == 0 {
		return "0%"
	}
	if v < 0.1 {
		return "<0.1%"
	}
	return fmt.Sprintf("%.1f%%", v)
}

// fmtCount adds thousands separators to a plain count.
func fmtCount(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// fmtTokens abbreviates a weighted token count the way web/src/lib/format.ts's
// fmtNumber does: 2 decimals into B or M, 1 decimal into k, otherwise a
// rounded plain integer.
func fmtTokens(f float64) string {
	switch {
	case f >= 1e9:
		return strconv.FormatFloat(f/1e9, 'f', 2, 64) + "B"
	case f >= 1e6:
		return strconv.FormatFloat(f/1e6, 'f', 2, 64) + "M"
	case f >= 1e3:
		return strconv.FormatFloat(f/1e3, 'f', 1, 64) + "k"
	default:
		return strconv.FormatFloat(f, 'f', 0, 64)
	}
}

func writeJSONOut(w io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(b))
	return err
}

// ---------------------------------------------------------------------
// store row -> routes wire type conversion
//
// internal/api/routes is deliberately dependency light (the GUI binary
// imports it and must not pull in internal/store), so it can't expose a
// constructor that takes a store row. The conversions below mirror the
// unexported ones in internal/api/server/attribution.go; duplicating this
// small amount of code here is the intended tradeoff so the CLI's JSON
// output stays byte-for-byte the same shape as the HTTP API's.
// ---------------------------------------------------------------------

func round2(f float64) float64 {
	return float64(int(f*100+0.5)) / 100
}

func round4(f float64) float64 {
	return float64(int64(f*10000+0.5)) / 10000
}

func attrGroupFromStore(g store.AttributionGroup, total float64) routes.AttrGroup {
	row := routes.AttrGroup{
		Key:           g.Key,
		Project:       g.Project,
		Cwd:           g.Cwd,
		Pct:           round2(g.Pct),
		MeasuredPct:   round2(g.MeasuredPct),
		EstimatedPct:  round2(g.EstimatedPct),
		PeakPct:       round2(g.PeakPct),
		CWTokens:      round2(g.CWTokens),
		RawTokens:     g.RawTokens,
		TurnCount:     g.TurnCount,
		Sessions:      g.Sessions,
		Windows:       g.Windows,
		FirstTSUnixMS: g.FirstTSUnixMS,
		LastTSUnixMS:  g.LastTSUnixMS,
	}
	if total > 0 {
		row.Share = round4(g.Pct / total)
	}
	return row
}

// attrWindowFromStore converts a window row, leaving Slices empty: the
// caller fills it in only when --slices is set. Note routes.AttrWindow has
// no field for a per-window tokens_per_pct_cw (the HTTP API only ever
// surfaces that as one aggregate figure), so unlike the human and
// --porcelain output, --json windows output cannot carry a per-window
// token rate; that's a limit of the reused type, not an oversight here.
func attrWindowFromStore(w store.LimitWindowRow) routes.AttrWindow {
	return routes.AttrWindow{
		StartUnixMS:   w.StartUnixMS,
		EndUnixMS:     w.EndUnixMS,
		ResetUnixMS:   w.ResetUnixMS,
		Inferred:      w.Inferred,
		Partial:       w.Partial,
		InProgress:    w.InProgress,
		HitCap:        w.HitCap,
		MeasuredPct:   round2(w.MeasuredPct),
		AttributedPct: round2(w.AttributedPct),
		PeakPct:       w.PeakPct,
		Slices:        []routes.AttrSlice{},
	}
}

func attrSliceFromStore(r store.AttributionRow) routes.AttrSlice {
	key, label := r.SessionUUID, r.SessionUUID
	if r.SessionUUID == attribute.Unattributed {
		key, label = attribute.Unattributed, ""
	}
	return routes.AttrSlice{
		Key:          key,
		Label:        label,
		Pct:          round2(r.MeasuredPct + r.EstimatedPct),
		MeasuredPct:  round2(r.MeasuredPct),
		EstimatedPct: round2(r.EstimatedPct),
		CWTokens:     round2(r.CWTokens),
		TurnCount:    r.TurnCount,
	}
}

func sessionAttributionFromStore(t *store.SessionPctTotals) routes.SessionAttribution {
	if t == nil {
		return routes.SessionAttribution{}
	}
	return routes.SessionAttribution{
		Cwd:          t.Cwd,
		WeekPct:      round2(t.WeekPct),
		FiveHPct:     round2(t.FiveHPct),
		FiveHPeakPct: round2(t.FiveHPeakPct),
		MeasuredPct:  round2(t.MeasuredPct),
		EstimatedPct: round2(t.EstimatedPct),
		Windows5h:    t.Windows5h,
		WindowsWeek:  t.WindowWeek,
	}
}

func sessionWindowSliceFromStore(r store.AttributionRow, w store.LimitWindowRow) routes.SessionWindowSlice {
	return routes.SessionWindowSlice{
		WindowStartUnixMS: r.WindowStartUnixMS,
		WindowEndUnixMS:   w.EndUnixMS,
		Inferred:          w.Inferred,
		Partial:           w.Partial,
		InProgress:        w.InProgress,
		Pct:               round2(r.MeasuredPct + r.EstimatedPct),
		MeasuredPct:       round2(r.MeasuredPct),
		EstimatedPct:      round2(r.EstimatedPct),
		WindowPct:         round2(w.AttributedPct),
		CWTokens:          round2(r.CWTokens),
		RawTokens:         r.RawTokens,
		TurnCount:         r.TurnCount,
		FirstTSUnixMS:     r.FirstTSUnixMS,
		LastTSUnixMS:      r.LastTSUnixMS,
	}
}
