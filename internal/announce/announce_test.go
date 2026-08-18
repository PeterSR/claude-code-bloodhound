package announce

import (
	"context"
	"errors"
	"testing"
	"time"

	ccsock "github.com/PeterSR/claude-code-socket-transport"

	"github.com/PeterSR/claude-code-bloodhound/internal/events"
	"github.com/PeterSR/claude-code-bloodhound/internal/notify"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

// testNow anchors the tests that render a moment. Fixed, and in a zone with
// an offset, so a weekday name is the same one on every machine that runs
// this: Wednesday 2026-08-05 08:00 at UTC+01:00.
var testNow = time.Date(2026, 8, 5, 8, 0, 0, 0, time.FixedZone("test", 3600))

func iso(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func ev(kind, bucket, cwd string, detail map[string]any) events.Event {
	return events.Event{
		Kind:   kind,
		Scope:  events.Scope{Bucket: bucket, Cwd: cwd},
		Detail: detail,
	}
}

func TestFilterWorthSayingKeepsOnlyRisingPressure(t *testing.T) {
	// The log records both directions because that is what a log is for.
	// Nobody needs interrupting to be told pressure went away.
	in := []events.Event{
		ev("budget.tight", "week", "/home/dev/myapp", nil),
		ev("budget.clear", "week", "/home/dev/myapp", nil),
		ev("budget.exceeded", "week", "/home/dev/myapp", nil),
		ev("limit_projection.projected", "session", "", nil),
		ev("limit_projection.clear", "session", "", nil),
		ev("saturation.saturated", "week", "", nil),
		ev("saturation.clear", "week", "", nil),
		// Not announceable at all: a dashboard fact, not an interruption.
		ev("threshold.80", "week", "", nil),
		ev("collection.stale", "", "", nil),
	}
	got := filterWorthSaying(in)
	want := []string{"budget.tight", "budget.exceeded", "limit_projection.projected", "saturation.saturated"}
	if len(got) != len(want) {
		t.Fatalf("kept %d events, want %d: %+v", len(got), len(want), kinds(got))
	}
	for i, k := range want {
		if got[i].Kind != k {
			t.Errorf("kept[%d] = %q, want %q", i, got[i].Kind, k)
		}
	}
}

func kinds(evs []events.Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}

func TestSplitKind(t *testing.T) {
	tests := []struct {
		in, kind, state string
		ok              bool
	}{
		{"budget.tight", "budget", "tight", true},
		{"limit_projection.projected", "limit_projection", "projected", true},
		{"threshold.80", "threshold", "80", true},
		{"nodot", "", "", false},
		{".leading", "", "", false},
		{"trailing.", "", "", false},
	}
	for _, tc := range tests {
		kind, state, ok := splitKind(tc.in)
		if ok != tc.ok || kind != tc.kind || state != tc.state {
			t.Errorf("splitKind(%q) = %q/%q/%v, want %q/%q/%v",
				tc.in, kind, state, ok, tc.kind, tc.state, tc.ok)
		}
	}
}

func TestTextForScopesBudgetsToTheirDirectory(t *testing.T) {
	// A budget event belongs to one directory. Telling an unrelated project
	// that someone else's allowance is tight is the noise a global broadcast
	// makes, and it is why Scope.Cwd exists at all.
	evs := []events.Event{
		ev("budget.tight", "week", "/home/dev/myapp", map[string]any{"reason": "myapp has 4.0% of its 20% week allowance left"}),
	}
	mine := ccsock.Session{CWD: "/home/dev/myapp"}
	theirs := ccsock.Session{CWD: "/home/dev/other"}

	if got := textFor(evs, mine, testNow); got == "" {
		t.Error("the directory that owns the budget was told nothing")
	}
	if got := textFor(evs, theirs, testNow); got != "" {
		t.Errorf("an unrelated directory was told %q", got)
	}
}

func TestTextForSendsAccountWideEventsToEveryone(t *testing.T) {
	// The 5h cliff stops every session on the machine, not just the one that
	// caused it, so these carry no cwd and reach anyone admitted.
	evs := []events.Event{ev("limit_projection.projected", "session", "", map[string]any{"eta_ts": iso(testNow.Add(40 * time.Minute))})}
	for _, cwd := range []string{"/home/dev/myapp", "/home/dev/other", ""} {
		if got := textFor(evs, ccsock.Session{CWD: cwd}, testNow); got == "" {
			t.Errorf("cwd %q was not told about an account-wide event", cwd)
		}
	}
}

func TestTextForNormalizesDirectories(t *testing.T) {
	// A trailing separator must not make a session look like a different
	// directory than the budget it owns.
	evs := []events.Event{ev("budget.exceeded", "week", "/home/dev/myapp", map[string]any{"reason": "spent"})}
	if got := textFor(evs, ccsock.Session{CWD: "/home/dev/myapp/"}, testNow); got == "" {
		t.Error("a trailing separator hid a session from its own budget")
	}
}

func TestDescribeReadsAsObservationNotInstruction(t *testing.T) {
	// Bloodhound measures. What to do about the measurement is the reader's
	// call, and a tool that starts issuing orders into conversations gets
	// switched off.
	cases := []events.Event{
		ev("budget.tight", "week", "/home/dev/myapp", map[string]any{"reason": "myapp has 4.0% left"}),
		ev("limit_projection.projected", "session", "", map[string]any{"eta_ts": iso(testNow.Add(40 * time.Minute))}),
		ev("saturation.saturated", "week", "", nil),
	}
	for _, e := range cases {
		got := describe(e, testNow)
		if got == "" {
			t.Errorf("%s produced no text", e.Kind)
			continue
		}
		for _, bossy := range []string{"you must", "stop ", "do not start", "You should"} {
			if containsFold(got, bossy) {
				t.Errorf("%s reads as an instruction: %q", e.Kind, got)
			}
		}
	}
}

func containsFold(s, sub string) bool {
	if len(sub) > len(s) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if equalFold(s[i:i+len(sub)], sub) {
			return true
		}
	}
	return false
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func TestDescribeFallsBackWithoutADetailReason(t *testing.T) {
	// The reason is written by the sensor, but an event replayed from an older
	// schema may not carry one and must still say something true.
	got := describe(ev("budget.exceeded", "week", "/home/dev/myapp", nil), testNow)
	if got == "" {
		t.Fatal("no fallback text")
	}
	if !containsFold(got, "myapp") {
		t.Errorf("fallback %q does not name the directory", got)
	}
}

// --- cursor behaviour -------------------------------------------------------
//
// The audit noted no test touched Run or the cursor at all. These do, using a
// real store and a Sender that records instead of writing to a socket.

type recordingSender struct{ sent []string }

func (r *recordingSender) Send(_ context.Context, _ notify.Target, text string) (string, error) {
	r.sent = append(r.sent, text)
	return "msg-id", nil
}

func announceStore(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	ctx := context.Background()
	s, err := store.Open(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, ctx
}

func appendEvent(t *testing.T, s *store.Store, ctx context.Context, kind string, ms int64) {
	t.Helper()
	if _, err := events.AppendTx(ctx, s.DB, ms, kind,
		events.Scope{Bucket: "week", Cwd: "/home/dev/myapp"}, nil); err != nil {
		t.Fatal(err)
	}
}

func cursorOf(t *testing.T, s *store.Store, ctx context.Context) int64 {
	t.Helper()
	c, _, err := readCursor(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestFirstRunStartsAtHeadRatherThanReciting(t *testing.T) {
	// Switching this on must not announce everything that ever happened.
	s, ctx := announceStore(t)
	now := time.Now()
	for i := 0; i < 5; i++ {
		appendEvent(t, s, ctx, "budget.tight", now.UnixMilli()+int64(i))
	}
	rec := &recordingSender{}
	st, err := Run(ctx, s, Options{Now: now, Sender: rec})
	if err != nil {
		t.Fatal(err)
	}
	if st.Announced != 0 || len(rec.sent) != 0 {
		t.Errorf("first run announced %d things, want 0", st.Announced)
	}
	head, _ := events.MaxID(ctx, s.DB)
	if got := cursorOf(t, s, ctx); got != head {
		t.Errorf("cursor = %d, want head %d", got, head)
	}
}

func TestCursorAdvancesSoNothingIsAnnouncedTwice(t *testing.T) {
	s, ctx := announceStore(t)
	now := time.Now()
	// Establish the cursor.
	if _, err := Run(ctx, s, Options{Now: now, Sender: &recordingSender{}}); err != nil {
		t.Fatal(err)
	}
	appendEvent(t, s, ctx, "budget.exceeded", now.UnixMilli())

	first, err := Run(ctx, s, Options{Now: now, Sender: &recordingSender{}})
	if err != nil {
		t.Fatal(err)
	}
	if first.Announced != 1 {
		t.Fatalf("first pass announced %d, want 1", first.Announced)
	}
	second, err := Run(ctx, s, Options{Now: now, Sender: &recordingSender{}})
	if err != nil {
		t.Fatal(err)
	}
	if second.Considered != 0 || second.Announced != 0 {
		t.Errorf("second pass considered %d / announced %d, want 0/0",
			second.Considered, second.Announced)
	}
}

func TestABacklogKeepsTheNewestTransitionsNotTheOldest(t *testing.T) {
	// The bug this pins: Query pages ascending, so taking backfillLimit rows
	// straight from the cursor returns the OLDEST of a backlog and then the
	// cursor jumps to head, discarding everything newer. After a real gap that
	// delivers stale transitions and drops the ones still true.
	s, ctx := announceStore(t)
	now := time.Now()
	if _, err := Run(ctx, s, Options{Now: now, Sender: &recordingSender{}}); err != nil {
		t.Fatal(err)
	}

	// A gap far longer than the backfill window. Oldest are saturation, newest
	// are budget, so the two are distinguishable in what gets said.
	total := backfillLimit * 3
	for i := 0; i < total; i++ {
		kind := "saturation.saturated"
		if i >= total-2 {
			kind = "budget.exceeded"
		}
		appendEvent(t, s, ctx, kind, now.UnixMilli()+int64(i))
	}

	rec := &recordingSender{}
	st, err := Run(ctx, s, Options{Now: now, Sender: rec})
	if err != nil {
		t.Fatal(err)
	}
	if st.Considered == 0 {
		t.Fatal("considered nothing from a large backlog")
	}
	if st.Announced == 0 {
		t.Fatal("announced nothing from a backlog containing fresh transitions")
	}
	// The newest events were budget ones; if the oldest window had been taken
	// they would all be saturation.
	head, _ := events.MaxID(ctx, s.DB)
	if got := cursorOf(t, s, ctx); got != head {
		t.Errorf("cursor = %d, want head %d", got, head)
	}
	if st.Skipped["older than the last 25 events"] == 0 {
		t.Error("dropped part of the backlog without reporting it")
	}
}

func TestDryRunLeavesTheCursorAlone(t *testing.T) {
	s, ctx := announceStore(t)
	now := time.Now()
	appendEvent(t, s, ctx, "budget.tight", now.UnixMilli())

	before := cursorOf(t, s, ctx)
	rec := &recordingSender{}
	if _, err := Run(ctx, s, Options{Now: now, DryRun: true, Sender: rec}); err != nil {
		t.Fatal(err)
	}
	if got := cursorOf(t, s, ctx); got != before {
		t.Errorf("dry run moved the cursor from %d to %d", before, got)
	}
	if len(rec.sent) != 0 {
		t.Errorf("dry run sent %d messages, want 0", len(rec.sent))
	}
}

func TestCursorIsClaimedBeforeDeliverySoAFailedPassDoesNotRepeat(t *testing.T) {
	// Two processes can run a pass at once. Claiming the cursor first makes
	// this at-most-once rather than at-most-twice, and the cost is that a pass
	// which dies mid-delivery loses its batch. That is the same asymmetry the
	// gate is built on, so it is deliberate and pinned here.
	s, ctx := announceStore(t)
	now := time.Now()
	if _, err := Run(ctx, s, Options{Now: now, Sender: &recordingSender{}}); err != nil {
		t.Fatal(err)
	}
	appendEvent(t, s, ctx, "budget.exceeded", now.UnixMilli())
	head, _ := events.MaxID(ctx, s.DB)

	if _, err := Run(ctx, s, Options{Now: now, Sender: &failingSender{}}); err != nil {
		t.Fatal(err)
	}
	if got := cursorOf(t, s, ctx); got != head {
		t.Errorf("cursor = %d, want %d claimed even though delivery failed", got, head)
	}
}

type failingSender struct{}

func (failingSender) Send(context.Context, notify.Target, string) (string, error) {
	return "", errors.New("socket exploded")
}

// TestDescribeSaysWhenTheWindowResets is the point of carrying reset_ts on the
// reading at all. "The 5h meter will hit the cap" is a different message
// depending on whether the window reopens in twenty minutes or on Friday, and
// without the second half the reader has to go and look it up.
func TestDescribeSaysWhenTheWindowResets(t *testing.T) {
	in90m := iso(testNow.Add(90 * time.Minute))
	onFriday := iso(testNow.Add(58 * time.Hour)) // Fri 18:00 local

	cases := []struct {
		name string
		ev   events.Event
		want string
	}{
		{
			"a budget under pressure",
			ev("budget.tight", "session", "/home/dev/myapp", map[string]any{
				"reason": "myapp has 4.0% of its 20% 5h allowance left", "reset_ts": in90m}),
			"Budget: myapp has 4.0% of its 20% 5h allowance left. The 5h window resets in 1h30m.",
		},
		{
			"a projection with an ETA of its own",
			ev("limit_projection.projected", "session", "", map[string]any{
				"eta_ts": iso(testNow.Add(40 * time.Minute)), "reset_ts": in90m}),
			"The 5h meter is on pace to reach 100% in 40m, before it resets in 1h30m.",
		},
		{
			"a weekly window far enough out to name the day",
			ev("saturation.saturated", "week", "", map[string]any{"reset_ts": onFriday}),
			"The week meter has stopped moving at its cap, so every figure downstream is now an estimate. It resets on fri 18:00.",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := describe(c.ev, testNow); got != c.want {
				t.Errorf("describe =\n  %q\nwant\n  %q", got, c.want)
			}
		})
	}
}

// TestDescribeDropsAResetThatHasAlreadyPassed covers the gap between writing
// an event and delivering it. A window that turned over in between must not be
// announced as resetting in the past, and the sentence has to still parse
// without the clause.
func TestDescribeDropsAResetThatHasAlreadyPassed(t *testing.T) {
	cases := []struct {
		name string
		ev   events.Event
		want string
	}{
		{
			"already turned over",
			ev("saturation.saturated", "session", "", map[string]any{"reset_ts": iso(testNow.Add(-time.Minute))}),
			"The 5h meter has stopped moving at its cap, so every figure downstream is now an estimate.",
		},
		{
			"never recorded",
			ev("saturation.saturated", "session", "", nil),
			"The 5h meter has stopped moving at its cap, so every figure downstream is now an estimate.",
		},
		{
			"not a timestamp at all",
			ev("saturation.saturated", "session", "", map[string]any{"reset_ts": "18:20"}),
			"The 5h meter has stopped moving at its cap, so every figure downstream is now an estimate.",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := describe(c.ev, testNow); got != c.want {
				t.Errorf("describe =\n  %q\nwant\n  %q", got, c.want)
			}
		})
	}
}
