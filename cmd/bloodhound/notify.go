package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/PeterSR/claude-code-bloodhound/internal/notify"
)

var (
	notifySession string
	notifyName    string
	notifyPID     int
	notifySelf    bool
	notifyList    bool
	notifyQuiet   bool
	notifyTimeout time.Duration
)

var notifyCmd = &cobra.Command{
	Use:   "notify [text]",
	Short: "Deliver a line of text into a running Claude Code session",
	Long: `Writes text into a session's inbox socket, where it appears in that
conversation the way a message from another Claude does.

This is the only way bloodhound can speak to a session that is mid-turn and
did not ask. The statusline waits to be looked at, ` + "`wait`" + ` and ` + "`when`" + ` need a
consumer already blocked, and a plugin monitor has to be armed at session
start. This reaches a session that did none of those things.

Text comes from the argument, or from stdin when there is no argument:

  bloodhound notify --self "ingest finished, 412 new turns"
  echo "week meter at 90%" | bloodhound notify --name myapp

The point of it is composing with the event log, so a threshold crossing
lands in the conversation of whoever needs to know:

  bloodhound when --for window.reset \
    --command 'bloodhound notify --session SESSION_UUID "5h window reset, go"'

Targeting: --self is the session that invoked bloodhound, read from the same
environment variable ` + "`bloodhound session`" + ` uses, so it only works when running
as a child of a session. --session takes a UUID and is the only target safe
against a recycled PID on its own. --name and --pid are conveniences; a name
can match several registry entries and only the live one is used.

Delivery is best effort by construction. The protocol is not documented by
Anthropic and can break on a Claude Code update; sessions older than v2.1.224
bind no inbox; native Windows has no cross-session messaging at all. A target
that cannot receive exits 3 rather than 1, so a hook can tell "nobody was
listening" from "the command was wrong". --quiet turns that into exit 0 for
opportunistic callers that should not fail a pipeline over it.

  bloodhound notify --list    show sessions that can receive right now`,
	Args:          cobra.MaximumNArgs(1),
	RunE:          runNotify,
	SilenceErrors: true,
}

func init() {
	notifyCmd.Flags().StringVar(&notifySession, "session", "", "target session UUID")
	notifyCmd.Flags().StringVar(&notifyName, "name", "", "target session name")
	notifyCmd.Flags().IntVar(&notifyPID, "pid", 0, "target session PID")
	notifyCmd.Flags().BoolVar(&notifySelf, "self", false, "target the session that invoked bloodhound")
	notifyCmd.Flags().BoolVar(&notifyList, "list", false, "list sessions that can receive right now, then exit")
	notifyCmd.Flags().BoolVar(&notifyQuiet, "quiet", false, "exit 0 instead of 3 when the target cannot receive")
	notifyCmd.Flags().DurationVar(&notifyTimeout, "timeout", 5*time.Second, "give up on the send after this long")
	rootCmd.AddCommand(notifyCmd)
}

// exitUndeliverable separates "nobody was listening" from a real error, so a
// hook firing into a session that has since closed does not look like a bug.
const exitUndeliverable = 3

func runNotify(cmd *cobra.Command, args []string) error {
	if notifyList {
		return runNotifyList(cmd.OutOrStdout())
	}

	target, err := notifyTarget()
	if err != nil {
		return err
	}

	text, err := notifyText(cmd.InOrStdin(), args)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), notifyTimeout)
	defer cancel()

	id, err := notify.New().Send(ctx, target, text)
	if err != nil {
		if notify.Undeliverable(err) {
			if notifyQuiet {
				return nil
			}
			fmt.Fprintln(os.Stderr, err)
			return exitCodeError{code: exitUndeliverable}
		}
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), id)
	return nil
}

// notifyTarget turns the flags into exactly one target, refusing a combination
// that names two different sessions rather than silently preferring one.
func notifyTarget() (notify.Target, error) {
	var t notify.Target
	set := 0
	if notifySession != "" {
		t.SessionID, set = notifySession, set+1
	}
	if notifyName != "" {
		t.Name, set = notifyName, set+1
	}
	if notifyPID != 0 {
		t.PID, set = notifyPID, set+1
	}
	if notifySelf {
		set++
		uuid := os.Getenv(claudeCodeSessionEnvVar)
		if uuid == "" {
			return t, fmt.Errorf("--self needs %s, which Claude Code sets only in processes it spawns; "+
				"pass --session explicitly when running outside a session", claudeCodeSessionEnvVar)
		}
		t.SessionID = uuid
	}
	switch set {
	case 0:
		return t, errors.New("pick a target: --self, --session, --name or --pid")
	case 1:
		return t, nil
	default:
		return t, errors.New("pick exactly one of --self, --session, --name or --pid")
	}
}

// notifyText reads the body from the argument or stdin. Stdin is the path that
// matters for hooks, where the text is usually produced by the command before
// it in the pipe.
func notifyText(stdin io.Reader, args []string) (string, error) {
	if len(args) == 1 {
		return args[0], nil
	}
	b, err := io.ReadAll(stdin)
	if err != nil {
		return "", fmt.Errorf("read text from stdin: %w", err)
	}
	text := strings.TrimRight(string(b), "\n")
	if strings.TrimSpace(text) == "" {
		return "", errors.New("no text: pass it as an argument or on stdin")
	}
	return text, nil
}

func runNotifyList(w io.Writer) error {
	live, err := notify.Reachable()
	if err != nil {
		return err
	}
	if len(live) == 0 {
		fmt.Fprintln(w, "No sessions are reachable.")
		fmt.Fprintln(w, "Sessions bind an inbox from Claude Code v2.1.224; native Windows has none.")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tNAME\tPID\tKIND\tSTATUS\tCWD")
	for _, s := range live {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n",
			s.SessionID, dashIfEmpty(s.Name), s.PID,
			dashIfEmpty(s.Kind), dashIfEmpty(s.Status), dashIfEmpty(s.CWD))
	}
	return tw.Flush()
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
