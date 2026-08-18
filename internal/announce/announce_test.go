package announce

import (
	"context"
	"errors"
	"testing"
	"time"

	ccsock "github.com/PeterSR/claude-code-socket-transport"

	"github.com/PeterSR/claude-code-bloodhound/internal/events"
	"github.com/PeterSR/claude-code-bloodhound/internal/notify"
	"github.com/PeterSR/claude-code-bloodhound/internal/projectconfig"
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
	got := filterWorthSaying(in, testNow)
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

	if got := compose(evs, mine, testNow, projectconfig.Config{}).Text; got == "" {
		t.Error("the directory that owns the budget was told nothing")
	}
	if got := compose(evs, theirs, testNow, projectconfig.Config{}).Text; got != "" {
		t.Errorf("an unrelated directory was told %q", got)
	}
}

func TestTextForSendsAccountWideEventsToEveryone(t *testing.T) {
	// The 5h cliff stops every session on the machine, not just the one that
	// caused it, so these carry no cwd and reach anyone admitted.
	evs := []events.Event{ev("limit_projection.projected", "session", "", map[string]any{"eta_ts": iso(testNow.Add(40 * time.Minute))})}
	for _, cwd := range []string{"/home/dev/myapp", "/home/dev/other", ""} {
		if got := compose(evs, ccsock.Session{CWD: cwd}, testNow, projectconfig.Config{}).Text; got == "" {
			t.Errorf("cwd %q was not told about an account-wide event", cwd)
		}
	}
}

func TestTextForNormalizesDirectories(t *testing.T) {
	// A trailing separator must not make a session look like a different
	// directory than the budget it owns.
	evs := []events.Event{ev("budget.exceeded", "week", "/home/dev/myapp", map[string]any{"reason": "spent"})}
	if got := compose(evs, ccsock.Session{CWD: "/home/dev/myapp/"}, testNow, projectconfig.Config{}).Text; got == "" {
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

// TestTextForAddsTheWakeupNoteOnce covers the opt-in tail. Three transitions
// landing together are one situation, and the same suggestion three times
// reads as a tool that has stopped paying attention.
func TestTextForAddsTheWakeupNoteOnce(t *testing.T) {
	in90m := iso(testNow.Add(90 * time.Minute))
	evs := []events.Event{
		ev("budget.tight", "session", "/home/dev/myapp", map[string]any{"reason": "myapp has 4.0% left", "reset_ts": in90m}),
		ev("limit_projection.projected", "session", "", map[string]any{"reset_ts": in90m}),
		ev("saturation.saturated", "session", "", map[string]any{"reset_ts": in90m}),
	}
	sess := ccsock.Session{CWD: "/home/dev/myapp"}

	got := compose(evs, sess, testNow, wantsNudge).Text
	if n := countOccurrences(got, nudgeNote(testNow.Add(90*time.Minute), "session")); n != 1 {
		t.Errorf("the note appears %d times, want exactly 1:\n%s", n, got)
	}
	if lines := splitLines(got); lines[len(lines)-1] != nudgeNote(testNow.Add(90*time.Minute), "session") {
		t.Errorf("the note is not the last line:\n%s", got)
	}
}

func TestTextForLeavesTheWakeupNoteOutUnlessAsked(t *testing.T) {
	// Off is the default, and the default has to be the quiet one: this puts
	// an extra line into somebody's conversation.
	evs := []events.Event{
		ev("saturation.saturated", "session", "", map[string]any{"reset_ts": iso(testNow.Add(90 * time.Minute))}),
	}
	got := compose(evs, ccsock.Session{CWD: "/home/dev/myapp"}, testNow, projectconfig.Config{}).Text
	if countOccurrences(got, nudgeNote(testNow.Add(90*time.Minute), "session")) != 0 {
		t.Errorf("the note went out to a directory that did not ask for it:\n%s", got)
	}
}

// TestTextForSkipsTheWakeupNoteWithoutAReset guards the dangling reference.
// The note points at "that reset", and without one in the text above there is
// nothing for it to point at and nothing to arm a wakeup for.
func TestTextForSkipsTheWakeupNoteWithoutAReset(t *testing.T) {
	evs := []events.Event{ev("saturation.saturated", "session", "", nil)}
	got := compose(evs, ccsock.Session{CWD: "/home/dev/myapp"}, testNow, wantsNudge).Text
	if countOccurrences(got, nudgeNote(testNow.Add(90*time.Minute), "session")) != 0 {
		t.Errorf("the note went out with no reset to point at:\n%s", got)
	}
}

func countOccurrences(s, sub string) int {
	n := 0
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			n++
		}
	}
	return n
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// TestRecommendationGoesOnlyToTheSessionItIsAbout is the routing the other
// events do not need. A budget belongs to a directory and a limit belongs to
// the whole account, but "your context has bloated" belongs to one
// conversation and means nothing in the window next to it.
func TestRecommendationGoesOnlyToTheSessionItIsAbout(t *testing.T) {
	e := ev("recommendation.compact", "", "", map[string]any{"reason": "recent turns are heavy"})
	e.Scope.Session = "session-abc12345"
	evs := []events.Event{e}
	on := wantsWriteup

	mine := ccsock.Session{SessionID: "session-abc12345", CWD: "/home/dev/myapp"}
	theirs := ccsock.Session{SessionID: "session-def67890", CWD: "/home/dev/myapp"}

	if got := compose(evs, mine, testNow, on).Text; got == "" {
		t.Error("the session it is about was told nothing")
	}
	if got := compose(evs, theirs, testNow, on).Text; got != "" {
		t.Errorf("another session in the same directory was told %q", got)
	}
}

// TestRecommendationNeedsTheDirectoryToAskForIt separates the two kinds of
// thing bloodhound says. A limit stopping every session on the machine is not
// something a directory gets to switch off; advice about how to work is.
func TestRecommendationNeedsTheDirectoryToAskForIt(t *testing.T) {
	e := ev("recommendation.compact", "", "", map[string]any{"reason": "recent turns are heavy"})
	e.Scope.Session = "session-abc12345"
	sess := ccsock.Session{SessionID: "session-abc12345", CWD: "/home/dev/myapp"}

	if got := compose([]events.Event{e}, sess, testNow, projectconfig.Config{}).Text; got != "" {
		t.Errorf("the nudge went out unasked: %q", got)
	}
	if got := compose([]events.Event{e}, sess, testNow, wantsWriteup).Text; got == "" {
		t.Error("the nudge was asked for and did not arrive")
	}
}

// TestFilterWorthSayingIgnoresTheCalmRecommendations keeps the dashboard
// states out of conversations. "ok" and "watch" are things to look at on a
// page, not things to interrupt someone with.
func TestFilterWorthSayingIgnoresTheCalmRecommendations(t *testing.T) {
	evs := []events.Event{
		ev("recommendation.ok", "", "", nil),
		ev("recommendation.watch", "", "", nil),
		ev("recommendation.compact", "", "", nil),
	}
	got := filterWorthSaying(evs, testNow)
	if len(got) != 1 || got[0].Kind != "recommendation.compact" {
		t.Errorf("kept %v, want only recommendation.compact", got)
	}
}

// The two opt-in shapes, spelled once. Both put an extra line into somebody's
// conversation, which is why neither is the default.
var (
	wantsWriteup = projectconfig.Config{WriteupNudge: projectconfig.Nudge{Enabled: true}}
	wantsNudge   = projectconfig.Config{Wakeup: projectconfig.Wakeup{Mode: projectconfig.WakeupNudge}}
	wantsResume  = projectconfig.Config{Wakeup: projectconfig.Wakeup{Mode: projectconfig.WakeupResume}}
	wantsCache   = projectconfig.Config{CacheNudge: projectconfig.Nudge{Enabled: true}}
)

// nudgeNote is the suggestion as the code under test renders it, rather than a
// copy of the sentence. A test that repeats the wording only pins the wording.
func nudgeNote(at time.Time, bucket string) string {
	line, _ := wakeupLine(wantsNudge, ccsock.Session{}, testNow, at, bucket, "")
	return line
}

// --- what the projection is allowed to interrupt for ------------------------

// TestAProjectionTwoDaysOutIsNotWorthInterrupting is the rule this was
// tightened for. The sensor is right that the meter is on pace to cap out
// before the week ends; it is a claim built by extrapolating one hour of burn
// across two days, and a reader at 56% cannot act on it.
func TestAProjectionTwoDaysOutIsNotWorthInterrupting(t *testing.T) {
	near := ev("limit_projection.projected", "session", "", map[string]any{
		"eta_ts": iso(testNow.Add(40 * time.Minute))})
	far := ev("limit_projection.projected", "week", "", map[string]any{
		"eta_ts": iso(testNow.Add(43 * time.Hour))})
	// No ETA at all fails open: the sensor still said the meter caps out
	// before the window resets, and that is worth hearing.
	blind := ev("limit_projection.projected", "week", "", nil)

	got := filterWorthSaying([]events.Event{near, far, blind}, testNow)
	if len(got) != 2 {
		t.Fatalf("kept %v, want the near one and the one with no ETA", kinds(got))
	}
	if got[0].Scope.Bucket != "session" || got[1].Detail != nil {
		t.Errorf("kept the wrong two: %+v", got)
	}
}

// TestTheProjectionLineSaysWhereTheMeterIs covers the other half of the same
// complaint. "On pace to cap out" reads very differently at 91% than at 56%,
// and a reader given only the projection has to go and look the reading up
// before they can judge it.
func TestTheProjectionLineSaysWhereTheMeterIs(t *testing.T) {
	e := ev("limit_projection.projected", "week", "", map[string]any{
		"pct":            float64(56),
		"burn_pct_per_h": 2.4,
		"eta_ts":         iso(testNow.Add(4 * time.Hour)),
		"reset_ts":       iso(testNow.Add(58 * time.Hour)),
	})
	want := "The week meter is at 56%, rising about 2.4%/h, and is on pace to reach 100% in 4h, before it resets on fri 18:00."
	if got := describe(e, testNow); got != want {
		t.Errorf("describe =\n  %q\nwant\n  %q", got, want)
	}
}

// --- the cache nudge --------------------------------------------------------

// TestTheCacheNudgeNeedsAsking is the same rule as the writeup nudge, and it
// matters more here: this is the one line that will wake a session that was
// not working.
func TestTheCacheNudgeNeedsAsking(t *testing.T) {
	e := ev("cache.expiring", "", "", map[string]any{"expires_in_s": float64(540)})
	e.Scope.Session = "session-abc12345"
	sess := ccsock.Session{SessionID: "session-abc12345", CWD: "/home/dev/myapp"}

	if got := compose([]events.Event{e}, sess, testNow, projectconfig.Config{}); got.Text != "" {
		t.Errorf("the nudge went out unasked: %q", got.Text)
	}
	got := compose([]events.Event{e}, sess, testNow, wantsCache)
	if got.Text == "" {
		t.Fatal("the nudge was asked for and did not arrive")
	}
	if !got.CacheNudge {
		t.Error("the message does not know it may wake a resting session")
	}
}

// TestOnlyTheExpiringBandIsWorthSaying separates the cheap moment from the
// bill already paid. "expired" means the prefix is gone and speaking now costs
// the full rebuild; "warm" means there is nothing to say yet.
func TestOnlyTheExpiringBandIsWorthSaying(t *testing.T) {
	evs := []events.Event{
		ev("cache.warm", "", "", nil),
		ev("cache.expiring", "", "", nil),
		ev("cache.expired", "", "", nil),
	}
	got := filterWorthSaying(evs, testNow)
	if len(got) != 1 || got[0].Kind != "cache.expiring" {
		t.Errorf("kept %v, want only cache.expiring", kinds(got))
	}
}

// --- the promise ------------------------------------------------------------

// TestResumeModePromisesRatherThanSuggests pins the difference between the two
// modes at the point a reader sees it. One says somebody should arm something;
// the other says bloodhound will do it, which is only sayable because a row
// gets written.
func TestResumeModePromisesRatherThanSuggests(t *testing.T) {
	evs := []events.Event{ev("budget.tight", "session", "/home/dev/myapp", map[string]any{
		"reason": "myapp has 4.0% left", "reset_ts": iso(testNow.Add(90 * time.Minute))})}
	sess := ccsock.Session{CWD: "/home/dev/myapp"}

	suggested := compose(evs, sess, testNow, wantsNudge)
	if suggested.Arm != nil {
		t.Error("nudge mode armed something; it is supposed to arm nothing")
	}

	promised := compose(evs, sess, testNow, wantsResume)
	if promised.Arm == nil {
		t.Fatal("resume mode promised nothing")
	}
	if promised.Arm.Bucket != "session" {
		t.Errorf("armed for %q, want the window that is closing", promised.Arm.Bucket)
	}
	if !containsFold(promised.Text, "bloodhound will write to this session") {
		t.Errorf("the promise is not in the text:\n%s", promised.Text)
	}
}

// TestNoPromiseIsMadeForAWindowDaysAway is the honesty check. "I will write to
// you on Thursday" is a calendar entry, not a resumption, and the session it
// would wake will have been closed for days.
func TestNoPromiseIsMadeForAWindowDaysAway(t *testing.T) {
	evs := []events.Event{ev("saturation.saturated", "week", "", map[string]any{
		"reset_ts": iso(testNow.Add(58 * time.Hour))})}
	got := compose(evs, ccsock.Session{CWD: "/home/dev/myapp"}, testNow, wantsResume)

	if got.Arm != nil {
		t.Error("promised a wakeup past the horizon")
	}
	if containsFold(got.Text, "bloodhound will write") {
		t.Errorf("promised in words what it did not promise in the table:\n%s", got.Text)
	}
	if got.Text == "" {
		t.Error("the warning itself went missing along with the promise")
	}
}

// TestAProjectSpeaksInItsOwnWords covers the templating end to end, including
// the thing it replaced: adding to a message rather than replacing it is
// {{.Text}} plus more, and it is the project that decides which side.
func TestAProjectSpeaksInItsOwnWords(t *testing.T) {
	cfg := projectconfig.Config{
		Pressure: projectconfig.Pressure{
			Message: "{{.Text}}\nPush the branch before you stop ({{.Dir}}, {{.Bucket}} back {{.Reset}}).",
		},
	}
	evs := []events.Event{ev("budget.tight", "session", "/home/dev/myapp", map[string]any{
		"reason": "myapp has 4.0% left", "reset_ts": iso(testNow.Add(90 * time.Minute))})}

	got := compose(evs, ccsock.Session{CWD: "/home/dev/myapp"}, testNow, cfg).Text
	if !containsFold(got, "myapp has 4.0% left") {
		t.Errorf("{{.Text}} lost the facts it was standing in for:\n%s", got)
	}
	if !containsFold(got, "Push the branch before you stop (myapp, 5h back in 1h30m).") {
		t.Errorf("the project's own line did not render:\n%s", got)
	}
}

// TestABrokenTemplateStillDeliversTheWarning is the failure this whole design
// is arranged around. A typo in the decoration must not be able to take the
// warning down with it, because the result is a silence and a silence reads as
// nothing being wrong.
func TestABrokenTemplateStillDeliversTheWarning(t *testing.T) {
	// Past Load's validation on purpose: this is the render-time half, for the
	// mistakes a parse cannot see.
	cfg := projectconfig.Config{Pressure: projectconfig.Pressure{Message: "{{.NotAField}}"}}
	evs := []events.Event{ev("budget.tight", "session", "/home/dev/myapp", map[string]any{
		"reason": "myapp has 4.0% left"})}

	got := compose(evs, ccsock.Session{CWD: "/home/dev/myapp"}, testNow, cfg)
	if !containsFold(got.Text, "myapp has 4.0% left") {
		t.Errorf("the warning was lost with the template:\n%s", got.Text)
	}
	if len(got.problems) != 1 {
		t.Errorf("problems = %v, want the failure recorded for the log", got.problems)
	}
}

// TestTheCapLeavesSessionScopedEventsAlone keeps a busy machine's nudges from
// crowding out the pressure warnings. A session-scoped event reaches exactly
// one conversation, so it is not competing for the same reader.
func TestTheCapLeavesSessionScopedEventsAlone(t *testing.T) {
	var in []events.Event
	for i := 0; i < 5; i++ {
		in = append(in, ev("budget.tight", "session", "/home/dev/myapp", nil))
	}
	for i := 0; i < 4; i++ {
		e := ev("cache.expiring", "", "", nil)
		e.Scope.Session = "session-abc12345"
		in = append(in, e)
	}
	st := Stats{Skipped: map[string]int{}}
	got := capped(in, &st)

	if len(got) != maxPerPass+4 {
		t.Fatalf("kept %d, want %d broad plus every session-scoped one", len(got), maxPerPass+4)
	}
	if st.Skipped["not the most recent transition"] != 2 {
		t.Errorf("skipped %v, want the two oldest broad events", st.Skipped)
	}
}
