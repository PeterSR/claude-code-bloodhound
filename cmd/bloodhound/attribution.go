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
// --porcelain apply to all of them. --by and --limit only mean something
// for the rollup (the bare `attribution` command), but cobra has nowhere
// better to declare them since all three share one persistent flag set.
var (
	attrBucket    string
	attrDays      int
	attrJSON      bool
	attrPorcelain bool
	attrBy        string
	attrLimit     int
	attrSlices    bool
)

var attributionCmd = &cobra.Command{
	Use:   "attribution",
	Short: "Show where the /usage limit meters actually went",
	Long: `Reads the session_attribution and limit_windows tables straight out of
SQLite (no daemon socket needed, so this works with the daemon stopped) and
reports how much of a limit meter each working directory or session
consumed.

With no subcommand this runs the rollup: one row per working directory (or
per session with --by session) across the requested lookback window.

  bloodhound attribution windows        the limit windows themselves
  bloodhound attribution session <id>   one session, window by window

--porcelain output is a stable parsing contract: tab separated, one record
per line, no header row. The rollup's columns, in order, are:

  key  pct  measured_pct  estimated_pct  peak_pct  share  sessions  windows
  turns  cw_tokens  raw_tokens  first_ts_unix_ms  last_ts_unix_ms

All percentages are printed at 4 decimal places (%.4f), cw_tokens and
tokens_per_pct_cw at 2 (%.2f), share as a 0 to 1 fraction at 4 decimals, and
everything else as a plain integer. An empty key (the unattributed
remainder) prints as a single "-".`,
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
	Use:   "session <id>",
	Short: "Show one session's cost against both limit meters, window by window",
	Long: `Resolves <id> as a full session UUID or a unique prefix of one (like a
git short SHA) and walks its cost against both the weekly and the 5 hour
meter, one line per limit window it touched.

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
	Args: cobra.ExactArgs(1),
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
		`rollup grouping, rollup only: "project" or "session"`)
	attributionCmd.PersistentFlags().IntVar(&attrLimit, "limit", 20,
		"max rollup rows in human mode, 0 for all (rollup only, ignored by --json/--porcelain)")

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
	case "project", "session":
		return attrBy, nil
	default:
		return "", fmt.Errorf(`invalid --by %q: must be "project" or "session"`, attrBy)
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := store.Open(ctx)
	if err != nil {
		return err
	}
	defer s.Close()

	since := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()

	groups, err := s.GroupAttribution(ctx, bucket, by, since)
	if err != nil {
		return err
	}
	windows, err := s.ListLimitWindows(ctx, bucket, since)
	if err != nil {
		return err
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

	w := cmd.OutOrStdout()
	switch {
	case attrJSON:
		return writeAttrRollupJSON(w, bucket, by, days, tokensPerPctCW,
			totalPct, measuredPct, estimatedPct, unattributedPct, groups)
	case attrPorcelain:
		return writeAttrRollupPorcelain(w, groups, totalPct)
	default:
		return writeAttrRollupHuman(w, bucket, by, days, len(windows), tokensPerPctCW,
			totalPct, measuredPct, estimatedPct, unattributedPct, groups, attrLimit)
	}
}

func writeAttrRollupHuman(w io.Writer, bucket, by string, days, windowCount int, tokensPerPctCW float64,
	totalPct, measuredPct, estimatedPct, unattributedPct float64, groups []store.AttributionGroup, limit int) error {
	if len(groups) == 0 {
		fmt.Fprintf(w, "nothing attributed yet for the %s bucket in the last %d days. Run `bloodhound aggregate` to build it.\n", bucket, days)
		return nil
	}

	fmt.Fprintf(w, "%s, by %s, last %d days across %d windows\n", bucketLabel(bucket), byLabel(by), days, windowCount)
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

	lastHeader := "WORKING DIR"
	if by == "session" {
		lastHeader = "SESSION"
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
	return g.Key
}

func writeAttrRollupJSON(w io.Writer, bucket, by string, days int, tokensPerPctCW,
	totalPct, measuredPct, estimatedPct, unattributedPct float64, groups []store.AttributionGroup) error {
	out := struct {
		Bucket          string             `json:"bucket"`
		By              string             `json:"by"`
		Days            int                `json:"days"`
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
		TokensPerPctCW:  round2(tokensPerPctCW),
		TotalPct:        round2(totalPct),
		MeasuredPct:     round2(measuredPct),
		EstimatedPct:    round2(estimatedPct),
		UnattributedPct: round2(unattributedPct),
		Groups:          []routes.AttrGroup{},
	}
	for _, g := range groups {
		out.Groups = append(out.Groups, attrGroupFromStore(g, totalPct))
	}
	return writeJSONOut(w, out)
}

func writeAttrRollupPorcelain(w io.Writer, groups []store.AttributionGroup, totalPct float64) error {
	for _, g := range groups {
		key := g.Key
		if key == "" {
			key = "-"
		}
		share := 0.0
		if totalPct > 0 {
			share = g.Pct / totalPct
		}
		fmt.Fprintf(w, "%s\t%.4f\t%.4f\t%.4f\t%.4f\t%.4f\t%d\t%d\t%d\t%.2f\t%d\t%d\t%d\n",
			key, g.Pct, g.MeasuredPct, g.EstimatedPct, g.PeakPct, share,
			g.Sessions, g.Windows, g.TurnCount, g.CWTokens, g.RawTokens,
			g.FirstTSUnixMS, g.LastTSUnixMS)
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := store.Open(ctx)
	if err != nil {
		return err
	}
	defer s.Close()

	uuid, err := s.ResolveSessionUUID(ctx, args[0])
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
	project := ""
	if totals != nil {
		project = totals.Project
	}
	fmt.Fprintf(w, "  %s\n", project)
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
	project := ""
	if totals != nil {
		project = totals.Project
	}
	out := struct {
		SessionUUID string                      `json:"session_uuid"`
		Project     string                      `json:"project"`
		Totals      routes.SessionAttribution   `json:"totals"`
		Week        []routes.SessionWindowSlice `json:"week"`
		FiveH       []routes.SessionWindowSlice `json:"five_h"`
	}{
		SessionUUID: uuid,
		Project:     project,
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
	if by == "session" {
		return "session"
	}
	return "working dir"
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
