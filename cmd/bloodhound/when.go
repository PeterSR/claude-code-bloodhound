package main

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/PeterSR/claude-code-bloodhound/internal/events"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

var (
	whenRun     string
	whenBucket  string
	whenSession string
	whenCwd     string
	whenExpires time.Duration
	whenNote    string
	whenList    bool
	whenAll     bool
	whenCancel  int64
	whenJSON    bool
)

var whenCmd = &cobra.Command{
	Use:   "when [kind-glob]",
	Short: "Register a one-shot command to run on the next matching event",
	Long: `Registers a shell command to run once, the next time a matching event is
recorded, and then be done.

  bloodhound when window.reset --run 'notify-send "5h window reset"'
  bloodhound when 'limit_projection.projected' --bucket session --run './park.sh'
  bloodhound when --list
  bloodhound when --cancel 7

Why this exists rather than just ` + "`bloodhound wait`" + `: wait needs a process to
stay alive until the event lands, and the thing that most wants to know when
the 5h window resets is a session that is about to stop working. It cannot hold
anything open. A one-shot moves the liveness requirement onto bloodhound, which
is already supervised.

It also fires late rather than never. A timer that sleeps until the reset
misses its tick when the laptop is closed over it; a registered one-shot fires
on the first reconcile after the machine wakes, and tells the handler how late
it was via BLOODHOUND_EVENT_LATE_S.

The event is delivered to the command on stdin as JSON, and never interpolated
into the command string. BLOODHOUND_EVENT_ID, _KIND, _BUCKET, _SESSION and
_LATE_S are set in its environment.

"Next" means strictly after the moment of registration, and --expires is
mandatory in spirit: a one-shot waiting for something that never comes is a
landmine, so it defaults to 24h.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runWhen,
}

func init() {
	whenCmd.Flags().StringVar(&whenRun, "run", "", "shell command to run once when the event lands")
	whenCmd.Flags().StringVar(&whenBucket, "bucket", "", "only match this limit bucket: session or week")
	whenCmd.Flags().StringVar(&whenSession, "session", "", "only match this session UUID")
	whenCmd.Flags().StringVar(&whenCwd, "cwd", "", "only match this working directory")
	whenCmd.Flags().DurationVar(&whenExpires, "expires", 24*time.Hour, "give up if nothing matches within this long")
	whenCmd.Flags().StringVar(&whenNote, "note", "", "free-text label, shown in --list")
	whenCmd.Flags().BoolVar(&whenList, "list", false, "list registered one-shots")
	whenCmd.Flags().BoolVar(&whenAll, "all", false, "with --list, include fired, expired and cancelled")
	whenCmd.Flags().Int64Var(&whenCancel, "cancel", 0, "cancel a pending one-shot by id")
	whenCmd.Flags().BoolVar(&whenJSON, "json", false, "emit JSON")
	rootCmd.AddCommand(whenCmd)
}

func runWhen(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := store.Open(ctx)
	if err != nil {
		return err
	}
	defer s.Close()

	now := time.Now()
	w := cmd.OutOrStdout()

	switch {
	case whenCancel > 0:
		ok, err := events.CancelHook(ctx, s.DB, whenCancel)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no pending one-shot with id %d", whenCancel)
		}
		fmt.Fprintf(w, "cancelled one-shot %d\n", whenCancel)
		return nil

	case whenList:
		hooks, err := events.ListHooks(ctx, s.DB, now, !whenAll)
		if err != nil {
			return err
		}
		if whenJSON {
			return writeJSONOut(w, hooks)
		}
		return writeHooksHuman(w, hooks, now)
	}

	if len(args) == 0 {
		return fmt.Errorf("need a kind glob (e.g. `bloodhound when window.reset --run ...`), or --list / --cancel")
	}
	if whenRun == "" {
		return fmt.Errorf("--run is required")
	}
	if whenExpires <= 0 {
		return fmt.Errorf("--expires must be positive: a one-shot that never expires is a landmine")
	}

	h, err := events.RegisterHook(ctx, s.DB, now, events.Hook{
		KindGlob:  args[0],
		Bucket:    whenBucket,
		Session:   whenSession,
		Cwd:       normalizeCwdFilter(whenCwd),
		Command:   whenRun,
		Note:      whenNote,
		ExpiresMS: now.Add(whenExpires).UnixMilli(),
	})
	if err != nil {
		return err
	}
	if whenJSON {
		return writeJSONOut(w, h)
	}
	fmt.Fprintf(w, "registered one-shot %d: %s -> %s\n", h.ID, h.KindGlob, h.Command)
	fmt.Fprintf(w, "  fires on the first match after event %d, expires %s\n",
		h.AfterEventID, time.UnixMilli(h.ExpiresMS).Local().Format("2006-01-02 15:04"))
	return nil
}

func writeHooksHuman(w io.Writer, hooks []events.Hook, now time.Time) error {
	if len(hooks) == 0 {
		fmt.Fprintln(w, "no one-shots registered.")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tKIND\tSCOPE\tEXPIRES\tCOMMAND")
	for _, h := range hooks {
		scope := h.Bucket
		if h.Session != "" {
			scope = h.Session
		}
		if scope == "" {
			scope = "-"
		}
		expires := time.UnixMilli(h.ExpiresMS).Local().Format("2006-01-02 15:04")
		status := h.Status
		if h.Status == events.HookFired && h.ExitCode != nil {
			status = fmt.Sprintf("fired(%d)", *h.ExitCode)
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", h.ID, status, h.KindGlob, scope, expires, h.Command)
	}
	return tw.Flush()
}
