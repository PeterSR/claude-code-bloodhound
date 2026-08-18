package main

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/PeterSR/claude-code-bloodhound/internal/budget"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
	"github.com/PeterSR/claude-code-bloodhound/internal/whenfmt"
)

var (
	wakeupsLimit   int
	wakeupsJSON    bool
	wakeupsSession string
)

var wakeupsCmd = &cobra.Command{
	Use:   "wakeups",
	Short: "Show the sessions bloodhound promised to write to when a window reopens",
	Long: `A project whose .bloodhound/config.json sets wakeup.mode to "resume" is asking
bloodhound to carry the wakeup rather than suggest one. When a session in that
directory is told to stop under pressure, it is noted down here, and when the
window it was waiting on reopens bloodhound writes to it again.

This is the only thing bloodhound does that outlives the message that caused
it, so it is worth being able to see. Pending rows are promises not yet kept.
Resolved ones say how they ended: delivered, superseded by a window that was
still shut, or expired because nothing was reachable before the reason went
stale.`,
	Args: cobra.NoArgs,
	RunE: runWakeups,
}

var wakeupsClearCmd = &cobra.Command{
	Use:   "clear",
	Short: "Drop pending promises without keeping them",
	Long: `Retires every pending wakeup, or only one session's with --session. The escape
hatch for anyone who does not want to be written to after all, and the thing to
reach for when a stale promise is pointing at a session that is long gone.

Resolved rows are history and are left alone.`,
	Args: cobra.NoArgs,
	RunE: runWakeupsClear,
}

func runWakeups(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	s, err := store.Open(ctx)
	if err != nil {
		return err
	}
	defer s.Close()

	rows, err := s.RecentWakeups(ctx, wakeupsLimit)
	if err != nil {
		return err
	}
	if wakeupsJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(rows)
	}

	if len(rows) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(),
			"No wakeups. A project asks for them with wakeup.mode = \"resume\" in its .bloodhound/config.json.")
		return nil
	}

	now := time.Now()
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SESSION\tDIR\tWINDOW\tARMED\tDUE\tSTATE")
	for _, r := range rows {
		state := r.Outcome
		if state == "" {
			state = "pending"
			if r.Attempts > 0 {
				state = fmt.Sprintf("pending (%d tries: %s)", r.Attempts, r.Note)
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s ago\t%s\t%s\n",
			shortSession(r.SessionUUID),
			budget.Label(r.Cwd),
			budget.BucketLabel(r.Bucket),
			whenfmt.Dur(now.Sub(r.Armed()).Milliseconds()),
			whenfmt.Phrase(r.Due(), now),
			state)
	}
	return w.Flush()
}

func runWakeupsClear(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	s, err := store.Open(ctx)
	if err != nil {
		return err
	}
	defer s.Close()

	n, err := s.CancelWakeups(ctx, wakeupsSession, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "dropped %d pending wakeup(s)\n", n)
	return nil
}

// shortSession keeps a UUID readable in a table. The first block is enough to
// tell two sessions apart by eye, and `--json` has the whole thing for anything
// that needs to match on it.
func shortSession(uuid string) string {
	if len(uuid) > 8 {
		return uuid[:8]
	}
	if uuid == "" {
		return "-"
	}
	return uuid
}

func init() {
	wakeupsCmd.Flags().IntVar(&wakeupsLimit, "limit", 20, "how many to show, newest first")
	wakeupsCmd.Flags().BoolVar(&wakeupsJSON, "json", false, "emit JSON instead of a table")
	wakeupsClearCmd.Flags().StringVar(&wakeupsSession, "session", "", "only this session's promises")
	wakeupsCmd.AddCommand(wakeupsClearCmd)
	rootCmd.AddCommand(wakeupsCmd)
}
