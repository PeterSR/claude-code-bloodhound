package main

import (
	"context"
	"errors"
	"fmt"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/PeterSR/claude-code-bloodhound/internal/events"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

var (
	waitFor     []string
	waitBucket  string
	waitSession string
	waitTimeout time.Duration
	waitSince   string
	waitJSON    bool
)

var waitCmd = &cobra.Command{
	Use:   "wait",
	Short: "Block until a matching event lands, then exit 0",
	Long: `Blocks until an event matching --for is recorded, prints it, and exits 0.
Exits 1 if the timeout elapses first, so it composes:

  bloodhound wait --for window.reset && notify-send "5h window reset"
  bloodhound wait --for 'limit_projection.projected' --bucket session --timeout 2h

By default it waits for events recorded from now on, ignoring history. Pass
--since to start from a cursor id or a duration instead.

This holds no state anywhere: it is a blocked process reading the same SQLite
every other subcommand reads, so it needs no daemon and survives a suspend
(the process is still here when the lid opens). What it cannot do is outlive
its own shell. When nothing can stay alive, register a one-shot with
` + "`bloodhound when`" + ` instead.`,
	Args: cobra.NoArgs,
	RunE: runWait,
	// A timeout is an ordinary outcome here, not a crash. Let main print the
	// one line rather than having cobra print its own on top of it.
	SilenceErrors: true,
}

func init() {
	waitCmd.Flags().StringArrayVar(&waitFor, "for", nil, "kind glob to wait for, repeatable (e.g. window.reset)")
	waitCmd.Flags().StringVar(&waitBucket, "bucket", "", "limit bucket: session or week")
	waitCmd.Flags().StringVar(&waitSession, "session", "", "session UUID")
	waitCmd.Flags().DurationVar(&waitTimeout, "timeout", time.Hour, "give up after this long (0 waits forever)")
	waitCmd.Flags().StringVar(&waitSince, "since", "", "start from a cursor id or duration instead of now")
	waitCmd.Flags().BoolVar(&waitJSON, "json", false, "emit the matched event as JSON")
	rootCmd.AddCommand(waitCmd)
}

func runWait(cmd *cobra.Command, args []string) error {
	if len(waitFor) == 0 {
		return fmt.Errorf("--for is required (e.g. --for window.reset)")
	}

	// Interrupt should exit quietly rather than through the error path: a
	// user pressing ctrl-c on a blocked wait has not hit a failure.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if waitTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, waitTimeout)
		defer cancel()
	}

	s, err := store.Open(ctx)
	if err != nil {
		return err
	}
	defer s.Close()

	now := time.Now()
	sinceID, sinceMS, err := parseSince(waitSince, now)
	if err != nil {
		return err
	}
	if waitSince == "" {
		head, err := events.MaxID(ctx, s.DB)
		if err != nil {
			return err
		}
		sinceID = head
	}

	f := events.Filter{
		SinceID: sinceID,
		SinceMS: sinceMS,
		Kinds:   waitFor,
		Bucket:  waitBucket,
		Session: waitSession,
		Limit:   1,
	}

	w := cmd.OutOrStdout()
	matched := false
	err = followEvents(ctx, w, s, f, 1, func(e events.Event) error {
		matched = true
		return writeEventLine(w, e, waitJSON)
	})
	if matched {
		return nil
	}
	// Only a deadline is a timeout. A ctrl-c is the user deciding to stop
	// waiting, and reporting that as "timed out after 1h0m0s" would be a lie.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("timed out after %s waiting for %v", waitTimeout, waitFor)
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return nil
	}
	return err
}
