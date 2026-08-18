package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/PeterSR/claude-code-bloodhound/internal/announce"
	"github.com/PeterSR/claude-code-bloodhound/internal/budget"
	"github.com/PeterSR/claude-code-bloodhound/internal/events"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

var budgetCmd = &cobra.Command{
	Use:   "budget",
	Short: "Set and inspect what a directory is allowed to spend",
	Long: `A budget is what one working directory is allowed to spend against a limit
bucket. Bloodhound measures against it and says so when the pressure changes;
it never enforces anything, because the thing acting on the number is an agent
that can read a sentence and decide for itself.

There are two rules and the difference is which number each watches.

  --spend  how much of the meter's movement THIS directory may itself cause,
           measured by attribution. Another project running concurrently does
           not consume it, and pausing does not either. This is the rule that
           only works because bloodhound attributes: two sessions told "max
           40%" against the global meter either stop at 80% between them or
           stop far short, because neither can tell its own spend from the
           other's.

  --meter  a reading on the shared meter to stop at, whoever moved it. No
           attribution involved. What it buys is headroom at the top, so
           whatever sits above the line stays available for work nobody
           governed, including everything you do by hand.

Set either or both, against the 5h window (--bucket session) or the week
(--bucket week, the default).

The 5h and weekly cliffs are watched whether or not you set anything. That is
what the limit_projection, threshold and saturation events already do, and
they need no configuration.`,
}

var (
	budgetSet    bool
	budgetSpend  float64
	budgetMeter  float64
	budgetBucket string
	budgetCwd    string
	budgetUntil  string
	budgetNote   string
	budgetAll    bool
	budgetJSON   bool
	budgetDryRun bool
)

func init() {
	budgetCmd.AddCommand(budgetSetCmd, budgetListCmd, budgetRevokeCmd, budgetStatusCmd, budgetAnnounceCmd)
	rootCmd.AddCommand(budgetCmd)

	for _, c := range []*cobra.Command{budgetSetCmd, budgetRevokeCmd, budgetStatusCmd} {
		c.Flags().StringVar(&budgetCwd, "cwd", "", "working directory the budget is for (default: the current one)")
		c.Flags().StringVar(&budgetBucket, "bucket", budget.BucketWeek, "limit bucket: session (5h) or week")
	}
	budgetSetCmd.Flags().Float64Var(&budgetSpend, "spend", 0, "points of the meter this directory may itself cause")
	budgetSetCmd.Flags().Float64Var(&budgetMeter, "meter", 0, "meter reading to stop at, whoever moved it")
	budgetSetCmd.Flags().StringVar(&budgetUntil, "until", "", "lease: reset, session, week, a duration (2h), a time (18:00) or a date")
	budgetSetCmd.Flags().StringVar(&budgetNote, "note", "", "why this budget exists")
	budgetListCmd.Flags().BoolVar(&budgetAll, "all", false, "include retired budgets")
	budgetListCmd.Flags().BoolVar(&budgetJSON, "json", false, "emit JSON")
	budgetStatusCmd.Flags().BoolVar(&budgetJSON, "json", false, "emit JSON")
	budgetAnnounceCmd.Flags().BoolVar(&budgetDryRun, "dry-run", false, "resolve and gate but send nothing")
}

var budgetSetCmd = &cobra.Command{
	Use:   "set",
	Short: "Set a budget for a directory",
	Long: `Sets a budget for a working directory and bucket, replacing whatever was
already in force there.

A budget stays until you revoke it unless you give it a lease with --until,
and a budget you know will evaporate is much easier to set than one you have
to remember to clean up:

  bloodhound budget set --spend 25 --until reset     until this bucket's window turns over
  bloodhound budget set --spend 25 --until session   until the 5h window turns over
  bloodhound budget set --meter 70 --until 2h        for the next two hours
  bloodhound budget set --spend 10 --until 18:00     until 18:00, or tomorrow if that passed
  bloodhound budget set --spend 10 --until 2026-08-20`,
	Args: cobra.NoArgs,
	RunE: runBudgetSet,
}

func runBudgetSet(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	cwd, err := resolveBudgetCwd()
	if err != nil {
		return err
	}
	if !budget.ValidBucket(budgetBucket) {
		return fmt.Errorf("--bucket must be %q or %q", budget.BucketSession, budget.BucketWeek)
	}

	s, err := store.Open(ctx)
	if err != nil {
		return err
	}
	defer s.Close()

	now := time.Now()
	windowEnds, err := budgetWindowEnds(ctx, s, now)
	if err != nil {
		return err
	}
	lease, err := budget.ParseUntil(budgetUntil, budgetBucket, now, windowEnds)
	if err != nil {
		return err
	}

	b := budget.Budget{
		Cwd:      cwd,
		Bucket:   budgetBucket,
		SpendPct: budgetSpend,
		MeterPct: budgetMeter,
		Note:     budgetNote,
		Leases:   []budget.Lease{lease},
	}
	saved, err := s.SetBudget(ctx, b, now)
	if err != nil {
		return err
	}

	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "Budget set for %s against the %s meter.\n", saved.Cwd, budget.BucketLabel(saved.Bucket))
	if saved.HasSpend() {
		fmt.Fprintf(w, "  spend  %.0f%% of the window, caused by this directory\n", saved.SpendPct)
	}
	if saved.HasMeter() {
		fmt.Fprintf(w, "  meter  stop when the shared meter reads %.0f%%\n", saved.MeterPct)
	}
	fmt.Fprintf(w, "  lease  %s\n", lease.Describe(now))
	return nil
}

var budgetListCmd = &cobra.Command{
	Use:   "list",
	Short: "List budgets",
	Args:  cobra.NoArgs,
	RunE:  runBudgetList,
}

func runBudgetList(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	s, err := store.Open(ctx)
	if err != nil {
		return err
	}
	defer s.Close()

	all, err := s.ListBudgets(ctx, budgetAll)
	if err != nil {
		return err
	}
	if budgetJSON {
		return writeJSON(cmd.OutOrStdout(), all)
	}
	if len(all) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No budgets set. `bloodhound budget set --help` explains the two rules.")
		return nil
	}
	now := time.Now()
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "DIRECTORY\tBUCKET\tSPEND\tMETER\tIN FORCE\tNOTE")
	for _, b := range all {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			b.Cwd, budget.BucketLabel(b.Bucket),
			pctOrDash(b.SpendPct), pctOrDash(b.MeterPct),
			inForce(b, now), b.Note)
	}
	return tw.Flush()
}

func inForce(b budget.Budget, now time.Time) string {
	if b.RetiredMS != nil {
		why := b.RetiredWhy
		if why == "" {
			why = "retired"
		}
		return why + " " + time.UnixMilli(*b.RetiredMS).Format("2 Jan 15:04")
	}
	for _, l := range b.Leases {
		if l.ExpiredMS == nil {
			return l.Describe(now)
		}
	}
	return "no live lease"
}

func pctOrDash(p float64) string {
	if p <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", p)
}

var budgetRevokeCmd = &cobra.Command{
	Use:   "revoke",
	Short: "Retire the budget for a directory",
	Args:  cobra.NoArgs,
	RunE:  runBudgetRevoke,
}

func runBudgetRevoke(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	cwd, err := resolveBudgetCwd()
	if err != nil {
		return err
	}
	s, err := store.Open(ctx)
	if err != nil {
		return err
	}
	defer s.Close()

	had, err := s.RevokeBudget(ctx, cwd, budgetBucket, time.Now())
	if err != nil {
		return err
	}
	if !had {
		fmt.Fprintf(cmd.OutOrStdout(), "No %s budget was set for %s.\n", budget.BucketLabel(budgetBucket), cwd)
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Revoked the %s budget for %s.\n", budget.BucketLabel(budgetBucket), cwd)
	return nil
}

var budgetStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the pressure a directory's budget is under",
	Args:  cobra.NoArgs,
	RunE:  runBudgetStatus,
}

func runBudgetStatus(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	cwd, err := resolveBudgetCwd()
	if err != nil {
		return err
	}
	s, err := store.Open(ctx)
	if err != nil {
		return err
	}
	defer s.Close()

	levels, err := budgetLevels(ctx, s, cwd)
	if err != nil {
		return err
	}
	if budgetJSON {
		return writeJSON(cmd.OutOrStdout(), levels)
	}
	w := cmd.OutOrStdout()
	if len(levels) == 0 {
		// A budget that exists but has no recorded level is waiting for the
		// next reconcile, which is not the same as having no budget at all.
		// Reporting both as "none" is how a user concludes their budget did
		// not save.
		set, err := s.ListBudgets(ctx, false)
		if err != nil {
			return err
		}
		mine := 0
		for _, b := range set {
			if b.Cwd == cwd {
				mine++
			}
		}
		if mine == 0 {
			fmt.Fprintf(w, "No budget set for %s.\n", cwd)
			return nil
		}
		fmt.Fprintf(w, "%d budget(s) set for %s, none evaluated yet.\n", mine, cwd)
		fmt.Fprintln(w, "Pressure is computed on the daemon's reconcile tick; run `bloodhound events reconcile` to force one.")
		return nil
	}
	for _, l := range levels {
		fmt.Fprintf(w, "%-8s %s\n", budget.BucketLabel(l.Scope.Bucket), l.State)
	}
	return nil
}

var budgetAnnounceCmd = &cobra.Command{
	Use:   "announce",
	Short: "Deliver pending pressure to sessions that are awake",
	Long: `Runs one delivery pass by hand. The daemon does this on its own schedule; this
is for seeing what it would say.

Only sessions that are mid-turn are told. A session sitting at its prompt is
left alone, because delivering to it would start a turn it was not going to
take, and if its cache has gone cold that turn re-pays the whole conversation
prefix before reading a word. --dry-run reports what would happen and sends
nothing.`,
	Args: cobra.NoArgs,
	RunE: runBudgetAnnounce,
}

func runBudgetAnnounce(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	s, err := store.Open(ctx)
	if err != nil {
		return err
	}
	defer s.Close()

	st, err := announce.Run(ctx, s, announce.Options{DryRun: budgetDryRun})
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	verb := "delivered"
	if budgetDryRun {
		verb = "would deliver"
	}
	fmt.Fprintf(w, "%d event(s) considered, %d worth saying, %s to %d session(s).\n",
		st.Considered, st.Announced, verb, st.Delivered)
	if len(st.Skipped) > 0 {
		fmt.Fprintln(w, "Skipped:")
		for reason, n := range st.Skipped {
			fmt.Fprintf(w, "  %-32s %d\n", reason, n)
		}
	}
	for _, e := range st.Errors {
		fmt.Fprintln(os.Stderr, "warning:", e)
	}
	return nil
}

// budgetLevels reads the recorded budget pressure for one directory straight
// out of event_levels, rather than recomputing it here. The reconciler owns
// that computation and the memory behind it, so asking it twice in two places
// is how the CLI and the GUI end up disagreeing.
func budgetLevels(ctx context.Context, s *store.Store, cwd string) ([]events.Level, error) {
	all, err := events.Levels(ctx, s.DB)
	if err != nil {
		return nil, err
	}
	var out []events.Level
	for _, l := range all {
		if l.Kind == "budget" && l.Scope.Cwd == cwd {
			out = append(out, l)
		}
	}
	return out, nil
}

// resolveBudgetCwd turns --cwd, or the process's own directory, into the
// absolute cleaned form used as the key.
func resolveBudgetCwd() (string, error) {
	c := budgetCwd
	if c == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("determine working directory: %w", err)
		}
		c = wd
	}
	return budget.NormalizeCwd(c)
}

// budgetWindowEnds reports each bucket's open-window end, which a window_reset
// lease needs at binding time.
func budgetWindowEnds(ctx context.Context, s *store.Store, now time.Time) (map[string]int64, error) {
	out := map[string]int64{}
	for bucket, back := range map[string]time.Duration{
		budget.BucketSession: 10 * time.Hour,
		budget.BucketWeek:    14 * 24 * time.Hour,
	} {
		windows, err := s.ListLimitWindows(ctx, budget.AttributeBucket(bucket), now.Add(-back).UnixMilli())
		if err != nil {
			return nil, err
		}
		for i := len(windows) - 1; i >= 0; i-- {
			if windows[i].InProgress {
				out[bucket] = windows[i].ResetUnixMS
				break
			}
		}
	}
	return out, nil
}

// writeJSON matches the indentation the other subcommands emit, so piping any
// of them into the same tooling behaves the same.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
