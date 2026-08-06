package events_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/events"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	s, err := store.Open(context.Background())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// scripted is a sensor whose answers are handed to it one reconcile at a time,
// which is how these tests drive a transition without needing a real poll.
type scripted struct {
	answers [][]events.Reading
	calls   int
}

func (s *scripted) sensor() events.Sensor {
	return events.Sensor{
		Name: "test/scripted",
		Read: func(ctx context.Context, w events.World) ([]events.Reading, error) {
			if s.calls >= len(s.answers) {
				return nil, nil
			}
			out := s.answers[s.calls]
			s.calls++
			return out, nil
		},
	}
}

func reading(state string) []events.Reading {
	return []events.Reading{{
		Kind:  "limit_projection",
		Scope: events.Scope{Bucket: "session"},
		State: state,
	}}
}

func reconcileN(t *testing.T, s *store.Store, n int, now time.Time) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := events.Reconcile(context.Background(), s.DB,
			events.World{DB: s.DB, Now: now.Add(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
}

func allEvents(t *testing.T, s *store.Store) []events.Event {
	t.Helper()
	evs, err := events.Query(context.Background(), s.DB, events.Filter{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	return evs
}

func TestReconcile_EmitsOncePerTransition(t *testing.T) {
	s := newTestStore(t)
	sc := &scripted{answers: [][]events.Reading{
		reading("clear"),
		reading("projected"),
		reading("projected"), // steady, must stay silent
		reading("clear"),
	}}
	events.SetRegistry([]events.Sensor{sc.sensor()})

	reconcileN(t, s, 4, time.Now())

	evs := allEvents(t, s)
	got := make([]string, len(evs))
	for i, e := range evs {
		got[i] = e.Kind
	}
	want := []string{
		"limit_projection.clear",
		"limit_projection.projected",
		"limit_projection.clear",
	}
	if len(got) != len(want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", got, want)
		}
	}
	if evs[1].PrevState != "clear" {
		t.Errorf("prev_state = %q, want %q", evs[1].PrevState, "clear")
	}
}

// The honesty property: a level that vanishes must not be silently forgotten,
// or a consumer goes on believing the last thing it heard. A projected limit
// disappearing because /usage extraction broke is exactly this case.
func TestReconcile_VanishingLevelBecomesUnknownRatherThanSilence(t *testing.T) {
	s := newTestStore(t)
	sc := &scripted{answers: [][]events.Reading{
		reading("projected"),
		reading(""), // sensor actively does not know any more
	}}
	events.SetRegistry([]events.Sensor{sc.sensor()})

	reconcileN(t, s, 2, time.Now())

	evs := allEvents(t, s)
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2", len(evs))
	}
	if evs[1].Kind != "limit_projection.unknown" {
		t.Errorf("kind = %q, want limit_projection.unknown", evs[1].Kind)
	}
	if evs[1].PrevState != "projected" {
		t.Errorf("prev_state = %q, want projected", evs[1].PrevState)
	}
}

// A fresh install has not polled yet. Emitting "unknown" for every level on
// the first tick would fill the log with noise before anything is knowable.
func TestReconcile_FirstEverUnknownIsSilent(t *testing.T) {
	s := newTestStore(t)
	sc := &scripted{answers: [][]events.Reading{reading(""), reading("")}}
	events.SetRegistry([]events.Sensor{sc.sensor()})

	reconcileN(t, s, 2, time.Now())

	if evs := allEvents(t, s); len(evs) != 0 {
		t.Fatalf("got %d events, want 0: %+v", len(evs), evs)
	}
}

// Omitting a reading is "I have nothing to say", which must not be confused
// with "I do not know". This is what stops a session ageing out of the recent
// window from emitting a spurious transition.
func TestReconcile_OmittedReadingLeavesLevelUntouched(t *testing.T) {
	s := newTestStore(t)
	sc := &scripted{answers: [][]events.Reading{
		reading("projected"),
		{}, // omitted entirely
		reading("projected"),
	}}
	events.SetRegistry([]events.Sensor{sc.sensor()})

	reconcileN(t, s, 3, time.Now())

	if evs := allEvents(t, s); len(evs) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(evs), evs)
	}
	lv, err := events.Levels(context.Background(), s.DB)
	if err != nil {
		t.Fatalf("levels: %v", err)
	}
	if len(lv) != 1 || lv[0].State != "projected" {
		t.Fatalf("levels = %+v, want one projected", lv)
	}
}

func TestReconcile_OneBrokenSensorDoesNotStopTheOthers(t *testing.T) {
	s := newTestStore(t)
	events.SetRegistry([]events.Sensor{
		{Name: "boom", Read: func(context.Context, events.World) ([]events.Reading, error) {
			return nil, os.ErrPermission
		}},
		(&scripted{answers: [][]events.Reading{reading("projected")}}).sensor(),
	})

	st, err := events.Reconcile(context.Background(), s.DB, events.World{DB: s.DB, Now: time.Now()})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if st.Emitted != 1 {
		t.Errorf("emitted = %d, want 1", st.Emitted)
	}
	if len(st.Errors) != 1 {
		t.Errorf("errors = %v, want one", st.Errors)
	}
}

func TestMatchKind(t *testing.T) {
	cases := []struct {
		globs []string
		kind  string
		want  bool
	}{
		{nil, "window.reset", true},
		{[]string{"window.reset"}, "window.reset", true},
		{[]string{"window.reset"}, "window.opened", false},
		{[]string{"limit_projection.*"}, "limit_projection.projected", true},
		{[]string{"limit_projection.*"}, "saturation.clear", false},
		{[]string{"*"}, "anything.at.all", true},
		{[]string{"nope", "window.*"}, "window.reset", true},
	}
	for _, c := range cases {
		if got := events.MatchKind(c.globs, c.kind); got != c.want {
			t.Errorf("MatchKind(%v, %q) = %v, want %v", c.globs, c.kind, got, c.want)
		}
	}
}

func TestQuery_FiltersAndOrders(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	for i, e := range []struct {
		kind  string
		scope events.Scope
	}{
		{"window.reset", events.Scope{Bucket: "session"}},
		{"window.reset", events.Scope{Bucket: "week"}},
		{"saturation.saturated", events.Scope{Bucket: "session"}},
	} {
		if _, err := events.AppendTx(ctx, s.DB, now+int64(i), e.kind, e.scope, nil); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	evs, err := events.Query(ctx, s.DB, events.Filter{Kinds: []string{"window.*"}, Bucket: "session"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(evs) != 1 || evs[0].Kind != "window.reset" || evs[0].Scope.Bucket != "session" {
		t.Fatalf("got %+v, want one session window.reset", evs)
	}

	all, err := events.Query(ctx, s.DB, events.Filter{})
	if err != nil {
		t.Fatalf("query all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d, want 3", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].ID <= all[i-1].ID {
			t.Fatalf("not ascending by id: %+v", all)
		}
	}

	since, err := events.Query(ctx, s.DB, events.Filter{SinceID: all[0].ID})
	if err != nil {
		t.Fatalf("query since: %v", err)
	}
	if len(since) != 2 {
		t.Fatalf("since = %d events, want 2", len(since))
	}
}

func TestHook_FiresOnceAndRecordsExitCode(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	events.SetRegistry(nil)

	marker := filepath.Join(t.TempDir(), "fired")
	h, err := events.RegisterHook(ctx, s.DB, now, events.Hook{
		KindGlob:  "window.reset",
		Command:   "cat > " + marker + "; exit 3",
		ExpiresMS: now.Add(time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	if _, err := events.AppendTx(ctx, s.DB, now.UnixMilli(), "window.reset",
		events.Scope{Bucket: "session"}, map[string]any{"pct": 91}); err != nil {
		t.Fatalf("append: %v", err)
	}

	st, err := events.Reconcile(ctx, s.DB, events.World{DB: s.DB, Now: now})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if st.HooksFired != 1 {
		t.Fatalf("fired = %d, want 1 (errors: %v)", st.HooksFired, st.Errors)
	}

	// The event reaches the command on stdin as JSON, never through its
	// argument string.
	body, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("marker: %v", err)
	}
	if len(body) == 0 || body[0] != '{' {
		t.Errorf("stdin payload = %q, want a JSON object", body)
	}

	hooks, err := events.ListHooks(ctx, s.DB, now, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(hooks) != 1 || hooks[0].Status != events.HookFired {
		t.Fatalf("hooks = %+v, want one fired", hooks)
	}
	if hooks[0].ExitCode == nil || *hooks[0].ExitCode != 3 {
		t.Errorf("exit code = %v, want 3", hooks[0].ExitCode)
	}
	if hooks[0].FiredEventID == nil {
		t.Error("fired_event_id not recorded")
	}
	_ = h

	// One shot means one shot: a second reconcile must not re-run it.
	st2, err := events.Reconcile(ctx, s.DB, events.World{DB: s.DB, Now: now})
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if st2.HooksFired != 0 {
		t.Errorf("second pass fired = %d, want 0", st2.HooksFired)
	}
}

// "Next" is measured from the moment of registration, so a reset that already
// happened must not satisfy a hook registered afterwards.
func TestHook_IgnoresEventsFromBeforeRegistration(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	events.SetRegistry(nil)

	if _, err := events.AppendTx(ctx, s.DB, now.UnixMilli(), "window.reset", events.Scope{}, nil); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := events.RegisterHook(ctx, s.DB, now, events.Hook{
		KindGlob:  "window.reset",
		Command:   "true",
		ExpiresMS: now.Add(time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	st, err := events.Reconcile(ctx, s.DB, events.World{DB: s.DB, Now: now})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if st.HooksFired != 0 {
		t.Errorf("fired = %d, want 0", st.HooksFired)
	}
}

// The suspend case: the event landed while nothing was watching, and the fire
// happens late rather than never.
func TestHook_FiresLateAfterAGap(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	reg := time.Now().Add(-6 * time.Hour)
	events.SetRegistry(nil)

	if _, err := events.RegisterHook(ctx, s.DB, reg, events.Hook{
		KindGlob:  "window.reset",
		Command:   "true",
		ExpiresMS: reg.Add(24 * time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	evTS := reg.Add(time.Hour)
	if _, err := events.AppendTx(ctx, s.DB, evTS.UnixMilli(), "window.reset", events.Scope{}, nil); err != nil {
		t.Fatalf("append: %v", err)
	}

	st, err := events.Reconcile(ctx, s.DB, events.World{DB: s.DB, Now: time.Now()})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if st.HooksFired != 1 {
		t.Fatalf("fired = %d, want 1 (errors: %v)", st.HooksFired, st.Errors)
	}
}

func TestHook_ExpiresAndCancels(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	events.SetRegistry(nil)

	if _, err := events.RegisterHook(ctx, s.DB, now, events.Hook{
		KindGlob:  "window.reset",
		Command:   "true",
		ExpiresMS: now.Add(-time.Minute).UnixMilli(), // already past
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	cancelMe, err := events.RegisterHook(ctx, s.DB, now, events.Hook{
		KindGlob:  "window.reset",
		Command:   "true",
		ExpiresMS: now.Add(time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatalf("register 2: %v", err)
	}

	st, err := events.Reconcile(ctx, s.DB, events.World{DB: s.DB, Now: now})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if st.HooksExpired != 1 {
		t.Errorf("expired = %d, want 1", st.HooksExpired)
	}

	ok, err := events.CancelHook(ctx, s.DB, cancelMe.ID)
	if err != nil || !ok {
		t.Fatalf("cancel = %v, %v", ok, err)
	}
	again, err := events.CancelHook(ctx, s.DB, cancelMe.ID)
	if err != nil {
		t.Fatalf("cancel again: %v", err)
	}
	if again {
		t.Error("cancelling twice reported success")
	}

	pending, err := events.ListHooks(ctx, s.DB, now, true)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("pending = %+v, want none", pending)
	}
}

// Retention must never drop an event a pending one-shot has not been matched
// against yet.
func TestPrune_RespectsThePendingFloor(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	old := now.Add(-30 * 24 * time.Hour).UnixMilli()

	for i := 0; i < 3; i++ {
		if _, err := events.AppendTx(ctx, s.DB, old+int64(i), "window.reset", events.Scope{}, nil); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	all := allEvents(t, s)
	floor := all[1].ID

	n, err := events.Prune(ctx, s.DB, now.Add(-7*24*time.Hour).UnixMilli(), floor)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned %d, want 1 (everything below the floor only)", n)
	}
	if left := allEvents(t, s); len(left) != 2 {
		t.Errorf("left %d events, want 2", len(left))
	}
}

// The flagship scenario, and the one the first cut got wrong: an 8h one-shot,
// a matching event at hour 7, and nothing running until hour 9. Sweeping
// expiries before matching swallowed the fire entirely.
func TestHook_FiresLateEvenWhenTheHookHasSinceExpired(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	events.SetRegistry(nil)

	reg := time.Now().Add(-9 * time.Hour)
	marker := filepath.Join(t.TempDir(), "fired")
	if _, err := events.RegisterHook(ctx, s.DB, reg, events.Hook{
		KindGlob:  "window.reset",
		Command:   "touch " + marker,
		ExpiresMS: reg.Add(8 * time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Landed at hour 7, while the hook was still live.
	if _, err := events.AppendTx(ctx, s.DB, reg.Add(7*time.Hour).UnixMilli(),
		"window.reset", events.Scope{Bucket: "session"}, nil); err != nil {
		t.Fatalf("append: %v", err)
	}

	// The machine wakes at hour 9, past the expiry.
	st, err := events.Reconcile(ctx, s.DB, events.World{DB: s.DB, Now: time.Now()})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if st.HooksFired != 1 {
		t.Fatalf("fired = %d, want 1 (expired = %d, errors %v)", st.HooksFired, st.HooksExpired, st.Errors)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("handler did not run: %v", err)
	}
}

// The other half of the same fix: an event that only arrives AFTER the hook's
// window closed must not resurrect it.
func TestHook_DoesNotFireForAnEventAfterItsExpiry(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	events.SetRegistry(nil)

	reg := time.Now().Add(-9 * time.Hour)
	if _, err := events.RegisterHook(ctx, s.DB, reg, events.Hook{
		KindGlob:  "window.reset",
		Command:   "true",
		ExpiresMS: reg.Add(time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := events.AppendTx(ctx, s.DB, reg.Add(5*time.Hour).UnixMilli(),
		"window.reset", events.Scope{}, nil); err != nil {
		t.Fatalf("append: %v", err)
	}

	st, err := events.Reconcile(ctx, s.DB, events.World{DB: s.DB, Now: time.Now()})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if st.HooksFired != 0 {
		t.Errorf("fired = %d, want 0", st.HooksFired)
	}
	if st.HooksExpired != 1 {
		t.Errorf("expired = %d, want 1", st.HooksExpired)
	}
}

// An empty prev_state has to mean exactly one thing: this is an edge. A level
// appearing for the first time says it came from "unknown", because it did.
func TestReconcile_FirstSeenLevelReportsUnknownAsItsPrevious(t *testing.T) {
	s := newTestStore(t)
	sc := &scripted{answers: [][]events.Reading{reading("clear")}}
	events.SetRegistry([]events.Sensor{sc.sensor()})

	reconcileN(t, s, 1, time.Now())

	evs := allEvents(t, s)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if evs[0].PrevState != "unknown" {
		t.Errorf("prev_state = %q, want unknown", evs[0].PrevState)
	}
}
