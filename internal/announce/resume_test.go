package announce

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	ccsock "github.com/PeterSR/claude-code-socket-transport"

	"github.com/PeterSR/claude-code-bloodhound/internal/notify"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

// The promise is the one thing bloodhound does that outlives the message that
// caused it, and it is delivered to a conversation nobody is watching. These
// pin the four ways that can go wrong: waking the wrong session, waking into a
// window that is still shut, waking so late the reason is stale, and giving up
// on the first refusal.

// onMachine replaces the registry read for the duration of a test.
func onMachine(t *testing.T, sessions ...ccsock.Session) *notify.Gate {
	t.Helper()
	prev := listSessions
	listSessions = func() ([]ccsock.Session, error) { return sessions, nil }
	t.Cleanup(func() { listSessions = prev })
	return &notify.Gate{AllowAtRest: true, AllowCold: true, MaxStatusAge: time.Hour}
}

// listening returns a session whose inbox socket really is bound, because the
// gate probes it for real and every refusal it makes is a refusal these tests
// are not about. Short path: a unix socket address overruns around 108
// characters, and t.TempDir() is already most of that.
func listening(t *testing.T, uuid string) ccsock.Session {
	t.Helper()
	path := filepath.Join(t.TempDir(), "i.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Skipf("no unix socket available: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return ccsock.Session{
		SessionID:  uuid,
		CWD:        "/home/dev/myapp",
		SocketPath: path,
		Status:     "shell",
	}
}

func promised(t *testing.T, s *store.Store, ctx context.Context, session, bucket string, armed time.Time, due time.Duration) int64 {
	t.Helper()
	id, err := s.ArmWakeup(ctx, store.SessionWakeup{
		SessionUUID: session,
		Cwd:         "/home/dev/myapp",
		Bucket:      bucket,
		Reason:      "Budget: myapp has 4.0% of its 20% 5h allowance left.",
		ArmedMS:     armed.UnixMilli(),
		DueMS:       armed.Add(due).UnixMilli(),
		ExpireMS:    armed.Add(due + resumeGrace).UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func outcomeOf(t *testing.T, s *store.Store, ctx context.Context, id int64) store.SessionWakeup {
	t.Helper()
	rows, err := s.RecentWakeups(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("wakeup %d vanished", id)
	return store.SessionWakeup{}
}

func TestAPromiseIsKeptWhenTheWindowReopens(t *testing.T) {
	s, ctx := announceStore(t)
	now := time.Now()
	sess := listening(t, "session-abc12345")
	gate := onMachine(t, sess)

	id := promised(t, s, ctx, sess.SessionID, "session", now.Add(-5*time.Hour), 5*time.Hour)

	rec := &recordingSender{}
	st, err := Run(ctx, s, Options{Now: now, Sender: rec, Gate: gate})
	if err != nil {
		t.Fatal(err)
	}
	if st.Resumed != 1 || len(rec.sent) != 1 {
		t.Fatalf("resumed %d, sent %d, want one of each", st.Resumed, len(rec.sent))
	}
	// It has to read cold. The session has been at its prompt for hours and
	// nobody remembers the warning this is the far side of.
	if !containsFold(rec.sent[0], "reopened") || !containsFold(rec.sent[0], "4.0%") {
		t.Errorf("the wakeup does not say what reopened or what stopped:\n%s", rec.sent[0])
	}
	if got := outcomeOf(t, s, ctx, id); got.Outcome != store.WakeupDelivered {
		t.Errorf("outcome = %q, want %q", got.Outcome, store.WakeupDelivered)
	}
}

func TestAPromiseIsNotKeptEarly(t *testing.T) {
	s, ctx := announceStore(t)
	now := time.Now()
	sess := listening(t, "session-abc12345")
	gate := onMachine(t, sess)

	promised(t, s, ctx, sess.SessionID, "session", now, 90*time.Minute)

	rec := &recordingSender{}
	st, err := Run(ctx, s, Options{Now: now, Sender: rec, Gate: gate})
	if err != nil {
		t.Fatal(err)
	}
	if st.Resumed != 0 || len(rec.sent) != 0 {
		t.Errorf("woke a session before its window reopened: %v", rec.sent)
	}
}

func TestTheFiveHourResetDoesNotWakeSomeoneTheWeekIsStillHolding(t *testing.T) {
	// The expensive mistake. Waking a session into a window that is still shut
	// hands it back the wall it stopped at, and charges the whole cold prefix
	// for the privilege.
	s, ctx := announceStore(t)
	now := time.Now()
	sess := listening(t, "session-abc12345")
	gate := onMachine(t, sess)

	fiveHour := promised(t, s, ctx, sess.SessionID, "session", now.Add(-5*time.Hour), 5*time.Hour)
	week := promised(t, s, ctx, sess.SessionID, "week", now.Add(-5*time.Hour), 9*time.Hour)

	rec := &recordingSender{}
	st, err := Run(ctx, s, Options{Now: now, Sender: rec, Gate: gate})
	if err != nil {
		t.Fatal(err)
	}
	if st.Resumed != 0 || len(rec.sent) != 0 {
		t.Errorf("woke a session while another window was still shut: %v", rec.sent)
	}
	if got := outcomeOf(t, s, ctx, fiveHour); got.Outcome != store.WakeupSuperseded {
		t.Errorf("outcome = %q, want %q", got.Outcome, store.WakeupSuperseded)
	}
	// The later promise is untouched and will carry it.
	if got := outcomeOf(t, s, ctx, week); got.Outcome != "" {
		t.Errorf("the week promise resolved as %q, want it still pending", got.Outcome)
	}
}

func TestAPromiseGoesStaleRatherThanArrivingLate(t *testing.T) {
	// Six hours after the window reopened, "you can pick up where you left
	// off" is an interruption with a stale reason attached rather than a
	// resumption of anything.
	s, ctx := announceStore(t)
	now := time.Now()
	sess := listening(t, "session-abc12345")
	gate := onMachine(t, sess)

	armed := now.Add(-24 * time.Hour)
	id := promised(t, s, ctx, sess.SessionID, "session", armed, 5*time.Hour)

	rec := &recordingSender{}
	st, err := Run(ctx, s, Options{Now: now, Sender: rec, Gate: gate})
	if err != nil {
		t.Fatal(err)
	}
	if st.Resumed != 0 || len(rec.sent) != 0 {
		t.Errorf("delivered a wakeup a day late: %v", rec.sent)
	}
	if got := outcomeOf(t, s, ctx, id); got.Outcome != store.WakeupExpired {
		t.Errorf("outcome = %q, want %q", got.Outcome, store.WakeupExpired)
	}
}

func TestAClosedSessionKeepsItsPromisePendingRatherThanLosingIt(t *testing.T) {
	// A registry read during a restart can miss a session that is about to be
	// back. Retiring on the first miss would turn a transient absence into a
	// broken promise; expire_ms is what eventually ends the waiting.
	s, ctx := announceStore(t)
	now := time.Now()
	gate := onMachine(t) // nothing running at all

	id := promised(t, s, ctx, "session-abc12345", "session", now.Add(-5*time.Hour), 5*time.Hour)

	rec := &recordingSender{}
	if _, err := Run(ctx, s, Options{Now: now, Sender: rec, Gate: gate}); err != nil {
		t.Fatal(err)
	}
	got := outcomeOf(t, s, ctx, id)
	if got.Outcome != "" {
		t.Errorf("outcome = %q, want it still pending", got.Outcome)
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want the miss recorded", got.Attempts)
	}
}

func TestADryRunPromisesAndResolvesNothing(t *testing.T) {
	s, ctx := announceStore(t)
	now := time.Now()
	sess := listening(t, "session-abc12345")
	gate := onMachine(t, sess)

	id := promised(t, s, ctx, sess.SessionID, "session", now.Add(-5*time.Hour), 5*time.Hour)

	rec := &recordingSender{}
	st, err := Run(ctx, s, Options{Now: now, Sender: rec, Gate: gate, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if st.Resumed != 1 {
		t.Errorf("resumed = %d, want the dry run to report what it would do", st.Resumed)
	}
	if len(rec.sent) != 0 {
		t.Errorf("a dry run sent %v", rec.sent)
	}
	if got := outcomeOf(t, s, ctx, id); got.Outcome != "" {
		t.Errorf("a dry run resolved the promise as %q", got.Outcome)
	}
}

func TestTwoWindowsReopeningTogetherIsOneWakeup(t *testing.T) {
	// One event to the session sitting there, not two. Waking it twice in a
	// minute pays the same cold prefix over again for the second half of a
	// sentence it already read.
	s, ctx := announceStore(t)
	now := time.Now()
	sess := listening(t, "session-abc12345")
	gate := onMachine(t, sess)

	armed := now.Add(-6 * time.Hour)
	early := promised(t, s, ctx, sess.SessionID, "session", armed, 5*time.Hour)
	late := promised(t, s, ctx, sess.SessionID, "week", armed, 5*time.Hour+time.Minute)

	rec := &recordingSender{}
	st, err := Run(ctx, s, Options{Now: now, Sender: rec, Gate: gate})
	if err != nil {
		t.Fatal(err)
	}
	if st.Resumed != 1 || len(rec.sent) != 1 {
		t.Fatalf("resumed %d, sent %d, want exactly one wakeup: %v", st.Resumed, len(rec.sent), rec.sent)
	}
	// The later window is the one that was actually holding the work up.
	if !containsFold(rec.sent[0], "week window has reopened") {
		t.Errorf("the wrong window spoke:\n%s", rec.sent[0])
	}
	if got := outcomeOf(t, s, ctx, late); got.Outcome != store.WakeupDelivered {
		t.Errorf("later promise outcome = %q, want %q", got.Outcome, store.WakeupDelivered)
	}
	if got := outcomeOf(t, s, ctx, early); got.Outcome != store.WakeupSuperseded {
		t.Errorf("earlier promise outcome = %q, want %q", got.Outcome, store.WakeupSuperseded)
	}
}
