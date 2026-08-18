// Package notify delivers a line of text into a running Claude Code session.
//
// Everything else bloodhound can say, it says passively. The weaverbird
// statusline waits to be looked at, `wait` and `when` need a consumer already
// blocked on them, and the plugin monitor channel has to be armed at session
// start. None of those reach a session that is mid-turn and did not ask. This
// package is the one path that does: it writes into the session's inbox
// socket, and the text lands in that conversation the way a message from
// another Claude does.
//
// The transport is github.com/PeterSR/claude-code-socket-transport, which
// implements a protocol Anthropic does not document. Three consequences shape
// the API here, and callers should not paper over them:
//
//   - It can break on any Claude Code update, with no deprecation. Delivery is
//     best effort. Nothing important may depend on it as the only path.
//   - Only sessions from Claude Code v2.1.224 bind an inbox at all, and native
//     Windows has no cross-session messaging whatsoever. A target that cannot
//     receive is an ordinary outcome, not a fault, so it comes back as a typed
//     error the caller can report quietly.
//   - Registry entries outlive the processes that wrote them. A PID can be
//     recycled and a socket path reused, so every send here verifies the entry
//     still carries the session it claims and probes the socket before
//     writing. The same liveness discipline the pigeon integration needed.
package notify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	ccsock "github.com/PeterSR/claude-code-socket-transport"
)

// FromName is the attribution the receiving conversation shows. Delivered
// messages appear under this name, so it should stay stable and recognisable.
const FromName = "bloodhound"

// probeTimeout bounds the liveness check on one socket. A registry entry for a
// dead session is common enough that this must stay short: it is paid once per
// candidate when resolving a name, and a slow probe would turn a routine miss
// into a visible stall.
const probeTimeout = 250 * time.Millisecond

// Errors callers are expected to handle rather than surface as failures.
var (
	// ErrNoSuchSession means nothing in the registry matched the target.
	ErrNoSuchSession = errors.New("no such session")
	// ErrNoInbox means the session exists but bound no inbox socket: it is
	// older than v2.1.224, running bare, or has messaging disabled.
	ErrNoInbox = errors.New("session has no inbox socket")
	// ErrUnreachable means the entry looks right but the socket did not answer,
	// which normally means the session is gone and the registry is stale.
	ErrUnreachable = errors.New("session inbox is not answering")
	// ErrEmptyText means there was nothing to say. A frame with empty content
	// is silently ignored by the receiver, so it is refused here instead.
	ErrEmptyText = errors.New("message text is empty")
)

// Target selects one session. Exactly one field should be set; SessionID is
// the only one safe against a recycled PID on its own, so prefer it.
type Target struct {
	SessionID string
	Name      string
	PID       int
}

// String renders the target for an error message.
func (t Target) String() string {
	switch {
	case t.SessionID != "":
		return "session " + t.SessionID
	case t.Name != "":
		return "name " + t.Name
	case t.PID != 0:
		return fmt.Sprintf("pid %d", t.PID)
	}
	return "no target"
}

// Sender delivers messages. The zero value is usable.
type Sender struct {
	client *ccsock.Client
}

// New returns a Sender.
func New() *Sender { return &Sender{client: ccsock.New()} }

func (s *Sender) c() *ccsock.Client {
	if s.client == nil {
		s.client = ccsock.New()
	}
	return s.client
}

// Send delivers text to the target and returns the message id the receiver
// assigned. It resolves the target through the registry first so it can verify
// the entry and probe the socket, rather than writing into a path that may
// belong to a different session than the one asked for.
func (s *Sender) Send(ctx context.Context, t Target, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", ErrEmptyText
	}
	sess, err := s.Resolve(t)
	if err != nil {
		return "", err
	}
	// SessionID is stamped on the frame as well as used for resolution: the
	// receiver drops a frame whose session id does not match its own, which
	// closes the window between the probe above and the write below.
	return s.c().Send(ctx, sess, ccsock.Message{
		Text:      text,
		FromName:  FromName,
		SessionID: sess.SessionID,
	})
}

// Resolve finds the live session a target names. It returns ErrNoInbox or
// ErrUnreachable rather than a match when the session cannot actually receive,
// so a caller can tell "wrong target" from "right target, no way in".
func (s *Sender) Resolve(t Target) (ccsock.Session, error) {
	sessions, err := ccsock.ListSessions()
	if err != nil {
		return ccsock.Session{}, fmt.Errorf("read session registry: %w", err)
	}

	var match *ccsock.Session
	for i := range sessions {
		sess := sessions[i]
		switch {
		case t.SessionID != "":
			if !sess.Verify(t.SessionID) {
				continue
			}
		case t.PID != 0:
			if sess.PID != t.PID {
				continue
			}
		case t.Name != "":
			if !strings.EqualFold(sess.Name, t.Name) {
				continue
			}
		default:
			return ccsock.Session{}, fmt.Errorf("%w: no target given", ErrNoSuchSession)
		}
		// A name can legitimately match several entries, most of them dead.
		// Keep looking for one that answers rather than failing on the first
		// stale hit, but remember the first match so the error can distinguish
		// "no such session" from "found it, it is gone".
		if match == nil {
			m := sess
			match = &m
		}
		if sess.SocketPath == "" {
			continue
		}
		if sess.Reachable(probeTimeout) {
			return sess, nil
		}
	}

	switch {
	case match == nil:
		return ccsock.Session{}, fmt.Errorf("%w: %s", ErrNoSuchSession, t)
	case match.SocketPath == "":
		return ccsock.Session{}, fmt.Errorf("%w: %s", ErrNoInbox, t)
	default:
		return ccsock.Session{}, fmt.Errorf("%w: %s", ErrUnreachable, t)
	}
}

// Reachable reports the sessions that can receive right now. Used by the CLI
// to list targets, and worth calling before offering a session as an option:
// the registry lists far more entries than are alive on a dev machine.
func Reachable() ([]ccsock.Session, error) {
	sessions, err := ccsock.ListSessions()
	if err != nil {
		return nil, fmt.Errorf("read session registry: %w", err)
	}
	live := make([]ccsock.Session, 0, len(sessions))
	for _, sess := range sessions {
		if sess.SocketPath == "" {
			continue
		}
		if sess.Reachable(probeTimeout) {
			live = append(live, sess)
		}
	}
	return live, nil
}

// Undeliverable reports whether err means the message could not be delivered
// for an ordinary reason: no such session, no inbox, or nothing answering.
// Callers that notify opportunistically should treat these as quiet skips and
// everything else as a real failure worth logging.
func Undeliverable(err error) bool {
	return errors.Is(err, ErrNoSuchSession) ||
		errors.Is(err, ErrNoInbox) ||
		errors.Is(err, ErrUnreachable)
}
