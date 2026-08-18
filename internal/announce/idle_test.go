package announce

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/events"
	"github.com/PeterSR/claude-code-bloodhound/internal/notify"
	"github.com/PeterSR/claude-code-bloodhound/internal/projectconfig"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

// The gate's default answer is "leave a resting session alone", and these are
// the two places the answer changes. Both go through Run rather than compose,
// because the decision is split between the message and the gate and testing
// either half alone would miss it.

// project writes a .bloodhound/config.json a session's cwd will resolve to,
// and points HOME above it so the walk has a stopping point.
func project(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "myapp")
	if err := os.MkdirAll(filepath.Join(dir, projectconfig.Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if body != "" {
		p := filepath.Join(dir, projectconfig.Dir, projectconfig.File)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	return dir
}

func sessionEvent(t *testing.T, s *store.Store, ctx context.Context, kind, uuid string, detail map[string]any) {
	t.Helper()
	if _, err := events.AppendTx(ctx, s.DB, time.Now().UnixMilli(), kind,
		events.Scope{Session: uuid}, detail); err != nil {
		t.Fatal(err)
	}
}

// primed writes a cursor so Run treats what follows as new rather than as the
// history a first run deliberately skips.
func primed(t *testing.T, s *store.Store, ctx context.Context) {
	t.Helper()
	if err := writeCursor(ctx, s, 0); err != nil {
		t.Fatal(err)
	}
}

func TestARestingSessionHearsTheCacheNudgeAndNothingElse(t *testing.T) {
	// The one line worth waking a resting session for. The cache is still
	// warm, so the turn it starts is paid at warm-cache rates, and the
	// alternative is that the same context is rebuilt from nothing later.
	s, ctx := announceStore(t)
	dir := project(t, `{"cache_nudge": {"enabled": true}}`)
	now := time.Now()

	sess := listening(t, "session-abc12345")
	sess.CWD = dir
	sess.Status = "shell" // at rest: the gate's default answer is no
	gate := onMachine(t, sess)
	gate.AllowAtRest = false // the real default; only the message may lift it

	primed(t, s, ctx)
	sessionEvent(t, s, ctx, "cache.expiring", sess.SessionID,
		map[string]any{"expires_in_s": float64(540)})

	rec := &recordingSender{}
	if _, err := Run(ctx, s, Options{Now: now, Sender: rec, Gate: gate}); err != nil {
		t.Fatal(err)
	}
	if len(rec.sent) != 1 {
		t.Fatalf("sent %d messages, want the nudge: %v", len(rec.sent), rec.sent)
	}
	if !containsFold(rec.sent[0], "last stretch") {
		t.Errorf("the wrong thing was said:\n%s", rec.sent[0])
	}
}

func TestARestingSessionIsStillLeftAloneAboutPressure(t *testing.T) {
	// Pressure alone does not lift the rule. Delivering here starts a turn
	// that would not otherwise have happened, and it is charged to the very
	// budget the warning is about.
	s, ctx := announceStore(t)
	dir := project(t, `{"cache_nudge": {"enabled": true}}`)
	now := time.Now()

	sess := listening(t, "session-abc12345")
	sess.CWD = dir
	sess.Status = "shell"
	gate := onMachine(t, sess)
	gate.AllowAtRest = false

	primed(t, s, ctx)
	if _, err := events.AppendTx(ctx, s.DB, now.UnixMilli(), "saturation.saturated",
		events.Scope{Bucket: "session"}, map[string]any{"pct": float64(95)}); err != nil {
		t.Fatal(err)
	}

	st, err := Run(ctx, s, Options{Now: now, Sender: &recordingSender{}, Gate: gate})
	if err != nil {
		t.Fatal(err)
	}
	if st.Delivered != 0 {
		t.Error("woke a resting session to tell it about the meter")
	}
	if st.Skipped[notify.SkipAtRest] != 1 {
		t.Errorf("skipped = %v, want the session left alone for being at rest", st.Skipped)
	}
}

func TestAPressureLineRidesAlongOnceTheWakeupIsPaidFor(t *testing.T) {
	// Once the cache nudge has decided to start a turn, the marginal cost of
	// the warning is the tokens of the sentence. Holding it back would be
	// paying for the delivery and then not making the delivery.
	s, ctx := announceStore(t)
	dir := project(t, `{"cache_nudge": {"enabled": true}}`)
	now := time.Now()

	sess := listening(t, "session-abc12345")
	sess.CWD = dir
	sess.Status = "shell"
	gate := onMachine(t, sess)
	gate.AllowAtRest = false

	primed(t, s, ctx)
	if _, err := events.AppendTx(ctx, s.DB, now.UnixMilli(), "saturation.saturated",
		events.Scope{Bucket: "session"}, map[string]any{"pct": float64(95)}); err != nil {
		t.Fatal(err)
	}
	sessionEvent(t, s, ctx, "cache.expiring", sess.SessionID,
		map[string]any{"expires_in_s": float64(540)})

	rec := &recordingSender{}
	if _, err := Run(ctx, s, Options{Now: now, Sender: rec, Gate: gate}); err != nil {
		t.Fatal(err)
	}
	if len(rec.sent) != 1 {
		t.Fatalf("sent %d messages, want one carrying both: %v", len(rec.sent), rec.sent)
	}
	if !containsFold(rec.sent[0], "stopped moving") || !containsFold(rec.sent[0], "last stretch") {
		t.Errorf("the batch lost one of its two lines:\n%s", rec.sent[0])
	}
}

func TestRunNotesTheSessionDownWhenTheProjectAsked(t *testing.T) {
	// End to end, because the promise is only made if the message actually
	// went out: arming a session that was never told to stop would mean
	// writing to it hours later about a warning it never heard.
	s, ctx := announceStore(t)
	dir := project(t, `{"wakeup": {"mode": "resume"}}`)
	now := time.Now()

	sess := listening(t, "session-abc12345")
	sess.CWD = dir
	sess.Status = "busy"
	sess.StatusUpdatedAt = now
	gate := onMachine(t, sess)
	gate.AllowAtRest, gate.AllowCold = false, false
	gate.Now = now

	primed(t, s, ctx)
	if _, err := events.AppendTx(ctx, s.DB, now.UnixMilli(), "saturation.saturated",
		events.Scope{Bucket: "session"}, map[string]any{
			"pct":      float64(95),
			"reset_ts": now.Add(90 * time.Minute).Format(time.RFC3339),
		}); err != nil {
		t.Fatal(err)
	}

	rec := &recordingSender{}
	st, err := Run(ctx, s, Options{Now: now, Sender: rec, Gate: gate})
	if err != nil {
		t.Fatal(err)
	}
	if st.Armed != 1 {
		t.Fatalf("armed = %d, want the session noted down", st.Armed)
	}
	pending, err := s.PendingWakeups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].SessionUUID != sess.SessionID {
		t.Fatalf("pending = %+v, want one promise for this session", pending)
	}
	if pending[0].Bucket != "session" {
		t.Errorf("armed for %q, want the window that is closing", pending[0].Bucket)
	}
	if !containsFold(rec.sent[0], "bloodhound will write to this session") {
		t.Errorf("the session was noted down without being told:\n%s", rec.sent[0])
	}
}
