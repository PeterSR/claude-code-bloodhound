package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/PeterSR/claude-code-bloodhound/internal/budget"
	"github.com/PeterSR/claude-code-bloodhound/internal/events"
	"github.com/PeterSR/claude-code-bloodhound/internal/events/sensors"
	"github.com/PeterSR/claude-code-bloodhound/internal/eventstate"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

var (
	eventsSince   string
	eventsKinds   []string
	eventsBucket  string
	eventsSession string
	eventsCwd     string
	eventsLimit   int
	eventsJSON    bool
	eventsLevels  bool
	eventsFollow  bool

	reconcileJSON bool
)

// followPoll is how often a blocked reader checks the table. There is no
// fsnotify and no socket involved, which is what lets --follow and `wait` work
// with the daemon stopped.
const followPoll = 2 * time.Second

var eventsCmd = &cobra.Command{
	Use:   "events",
	Short: "Read the event log: state transitions with a cursor",
	Long: `Prints events the collector recorded, oldest first, each with the state it
transitioned from.

Reads SQLite directly like every other subcommand, so it works with the daemon
stopped. Every response carries the same poll-freshness fields ` + "`now --json`" + `
does, because an empty result from a database nobody has written to in an hour
means something very different from an empty result from a healthy one, and a
consumer must be able to tell those apart.

--since takes either a cursor id (an integer) or a duration ("30m", "2h").
--levels prints what is true right now instead of the transitions, which is
what a short-lived consumer like a statusline wants: it holds no cursor and
does not care how the current state was reached.`,
	Args: cobra.NoArgs,
	RunE: runEvents,
}

var eventsReconcileCmd = &cobra.Command{
	Use:   "reconcile",
	Short: "Run the sensors once and record any transitions",
	Long: `Runs every registered sensor, diffs what they report against the recorded
levels, appends an event per transition, then fires any one-shot hooks that
matched.

The daemon calls this on a tick. It is exposed here so a cron-scheduled install
behaves identically, since both go through the same function and neither needs
a socket.`,
	Args: cobra.NoArgs,
	RunE: runEventsReconcile,
}

func init() {
	eventsCmd.Flags().StringVar(&eventsSince, "since", "", "cursor id, or a duration like 30m")
	eventsCmd.Flags().StringArrayVar(&eventsKinds, "kind", nil, "kind glob, repeatable (e.g. 'limit_projection.*')")
	eventsCmd.Flags().StringVar(&eventsBucket, "bucket", "", "limit bucket: session or week")
	eventsCmd.Flags().StringVar(&eventsSession, "session", "", "session UUID")
	eventsCmd.Flags().StringVar(&eventsCwd, "cwd", "", "working directory (budget events are scoped to one)")
	eventsCmd.Flags().IntVar(&eventsLimit, "limit", 0, "max events to return (default 500)")
	eventsCmd.Flags().BoolVar(&eventsJSON, "json", false, "emit JSON")
	eventsCmd.Flags().BoolVar(&eventsLevels, "levels", false, "print current levels instead of transitions")
	eventsCmd.Flags().BoolVar(&eventsFollow, "follow", false, "block and print each new event as it lands")

	eventsReconcileCmd.Flags().BoolVar(&reconcileJSON, "json", false, "emit JSON")

	eventsCmd.AddCommand(eventsReconcileCmd)
	rootCmd.AddCommand(eventsCmd)
}

// parseSince accepts a cursor id or a duration. Returns (sinceID, sinceMS).
func parseSince(arg string, now time.Time) (int64, int64, error) {
	if arg == "" {
		return 0, 0, nil
	}
	if id, err := strconv.ParseInt(arg, 10, 64); err == nil {
		return id, 0, nil
	}
	d, err := time.ParseDuration(arg)
	if err != nil {
		return 0, 0, fmt.Errorf("--since %q: want a cursor id or a duration like 30m", arg)
	}
	if d < 0 {
		d = -d
	}
	return 0, now.Add(-d).UnixMilli(), nil
}

func runEvents(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	s, err := store.Open(ctx)
	if err != nil {
		return err
	}
	defer s.Close()

	now := time.Now()
	sinceID, sinceMS, err := parseSince(eventsSince, now)
	if err != nil {
		return err
	}

	f := events.Filter{
		SinceID: sinceID,
		SinceMS: sinceMS,
		Kinds:   eventsKinds,
		Bucket:  eventsBucket,
		Session: eventsSession,
		Cwd:     eventsCwd,
		Limit:   eventsLimit,
	}

	w := cmd.OutOrStdout()

	if eventsFollow {
		// Default to "from here on" rather than replaying history, which is
		// what a follower almost always means. An explicit --since overrides.
		if eventsSince == "" {
			head, err := events.MaxID(ctx, s.DB)
			if err != nil {
				return err
			}
			f.SinceID = head
		}
		return followEvents(ctx, w, s, f, 0, func(e events.Event) error {
			return writeEventLine(w, e, eventsJSON)
		})
	}

	env, err := eventstate.Compute(ctx, s, now, f, eventsLevels)
	if err != nil {
		return err
	}
	if eventsJSON {
		return writeJSONOut(w, env)
	}
	return writeEventsHuman(w, env, eventsLevels)
}

// followEvents blocks, polling the table and calling emit for each new event
// in order. Stops after maxN emits when maxN > 0.
func followEvents(ctx context.Context, w io.Writer, s *store.Store, f events.Filter, maxN int, emit func(events.Event) error) error {
	cursor := f.SinceID
	seen := 0
	tick := time.NewTicker(followPoll)
	defer tick.Stop()

	for {
		// Read the head before the filtered query. Every event at or below it
		// has been considered by the time the query returns, so the cursor can
		// skip the non-matching tail rather than rescanning it every tick.
		// SQLite serialises writers, so ids are committed in ascending order
		// and this watermark cannot step over an event still in flight.
		head, err := events.MaxID(ctx, s.DB)
		if err != nil {
			return err
		}

		q := f
		q.SinceID = cursor
		evs, err := events.Query(ctx, s.DB, q)
		if err != nil {
			return err
		}
		for _, e := range evs {
			if err := emit(e); err != nil {
				return err
			}
			cursor = e.ID
			seen++
			if maxN > 0 && seen >= maxN {
				return nil
			}
		}
		// Only safe to jump the watermark when the page was not truncated;
		// a full page means there may be more matches below head.
		limit := q.Limit
		if limit <= 0 {
			limit = events.DefaultLimit
		}
		if len(evs) < limit && head > cursor {
			cursor = head
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func writeEventLine(w io.Writer, e events.Event, asJSON bool) error {
	if asJSON {
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(w, string(b))
		return err
	}
	_, err := fmt.Fprintf(w, "%d\t%s\t%s%s\n", e.ID, e.TSISO, e.Kind, scopeSuffix(e.Scope))
	return err
}

func scopeSuffix(sc events.Scope) string {
	if r := renderScope(sc); r != "" {
		return " [" + r + "]"
	}
	return ""
}

// renderScope picks the one identifier that says most about where an event
// happened. Directory beats bucket because a budget event carries both and the
// bucket alone makes two directories' events indistinguishable in a listing;
// session beats everything because it is the narrowest.
//
// A budget event renders as "myapp:week" rather than the full path, which
// would push every other column off the terminal. The full path is still in
// --json, and --cwd takes it.
func renderScope(sc events.Scope) string {
	switch {
	case sc.Session != "":
		return sc.Session
	case sc.Cwd != "" && sc.Bucket != "":
		return budget.Label(sc.Cwd) + ":" + sc.Bucket
	case sc.Cwd != "":
		return budget.Label(sc.Cwd)
	case sc.Bucket != "":
		return sc.Bucket
	}
	return ""
}

func writeEventsHuman(w io.Writer, env *eventstate.Response, withLevels bool) error {
	if env.LastPoll == nil {
		fmt.Fprintln(w, "no /usage observation captured yet, so nothing has been evaluated.")
	} else if env.Stale {
		fmt.Fprintf(w, "warning: last poll is %ds old (stale after %ds). Absence of events below may mean absence of collection.\n\n",
			env.LastPoll.AgeS, env.StaleAfterS)
	}

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if withLevels {
		fmt.Fprintln(tw, "LEVEL\tSCOPE\tSTATE\tSINCE")
		for _, l := range env.Levels {
			scope := renderScope(l.Scope)
			if scope == "" {
				scope = "-"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", l.Kind, scope, l.State,
				time.UnixMilli(l.SinceMS).Local().Format("2006-01-02 15:04"))
		}
		if len(env.Levels) == 0 {
			fmt.Fprintln(tw, "(none yet)\t\t\t")
		}
		return tw.Flush()
	}

	fmt.Fprintln(tw, "ID\tWHEN\tKIND\tFROM\tSCOPE")
	for _, e := range env.Events {
		from := e.PrevState
		if from == "" {
			from = "-"
		}
		scope := renderScope(e.Scope)
		if scope == "" {
			scope = "-"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", e.ID,
			time.UnixMilli(e.TSUnixMS).Local().Format("2006-01-02 15:04"), e.Kind, from, scope)
	}
	if len(env.Events) == 0 {
		fmt.Fprintln(tw, "(no events)\t\t\t\t")
	}
	return tw.Flush()
}

func runEventsReconcile(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s, err := store.Open(ctx)
	if err != nil {
		return err
	}
	defer s.Close()

	st, err := sensors.Run(ctx, s, time.Now())
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	if reconcileJSON {
		return writeJSONOut(w, st)
	}
	fmt.Fprintf(w, "sensors=%d readings=%d emitted=%d hooks_fired=%d hooks_expired=%d\n",
		st.SensorsRun, st.ReadingsSeen, st.Emitted, st.HooksFired, st.HooksExpired)
	for _, e := range st.Errors {
		fmt.Fprintf(w, "  warn: %s\n", e)
	}
	return nil
}
