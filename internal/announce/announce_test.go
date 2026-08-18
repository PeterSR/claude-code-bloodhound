package announce

import (
	"testing"

	ccsock "github.com/PeterSR/claude-code-socket-transport"

	"github.com/PeterSR/claude-code-bloodhound/internal/events"
)

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

	if got := textFor(evs, mine); got == "" {
		t.Error("the directory that owns the budget was told nothing")
	}
	if got := textFor(evs, theirs); got != "" {
		t.Errorf("an unrelated directory was told %q", got)
	}
}

func TestTextForSendsAccountWideEventsToEveryone(t *testing.T) {
	// The 5h cliff stops every session on the machine, not just the one that
	// caused it, so these carry no cwd and reach anyone admitted.
	evs := []events.Event{ev("limit_projection.projected", "session", "", map[string]any{"eta_ts": "18:20"})}
	for _, cwd := range []string{"/home/dev/myapp", "/home/dev/other", ""} {
		if got := textFor(evs, ccsock.Session{CWD: cwd}); got == "" {
			t.Errorf("cwd %q was not told about an account-wide event", cwd)
		}
	}
}

func TestTextForNormalizesDirectories(t *testing.T) {
	// A trailing separator must not make a session look like a different
	// directory than the budget it owns.
	evs := []events.Event{ev("budget.exceeded", "week", "/home/dev/myapp", map[string]any{"reason": "spent"})}
	if got := textFor(evs, ccsock.Session{CWD: "/home/dev/myapp/"}); got == "" {
		t.Error("a trailing separator hid a session from its own budget")
	}
}

func TestDescribeReadsAsObservationNotInstruction(t *testing.T) {
	// Bloodhound measures. What to do about the measurement is the reader's
	// call, and a tool that starts issuing orders into conversations gets
	// switched off.
	cases := []events.Event{
		ev("budget.tight", "week", "/home/dev/myapp", map[string]any{"reason": "myapp has 4.0% left"}),
		ev("limit_projection.projected", "session", "", map[string]any{"eta_ts": "18:20"}),
		ev("saturation.saturated", "week", "", nil),
	}
	for _, e := range cases {
		got := describe(e)
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
	got := describe(ev("budget.exceeded", "week", "/home/dev/myapp", nil))
	if got == "" {
		t.Fatal("no fallback text")
	}
	if !containsFold(got, "myapp") {
		t.Errorf("fallback %q does not name the directory", got)
	}
}
