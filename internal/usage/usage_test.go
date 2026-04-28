package usage

import (
	"strings"
	"testing"
	"time"
)

// Synthetic /usage panel mirroring real TUI output (run-together labels,
// no-whitespace resets). Real samples are kept in .agent-workspace and
// must NEVER appear in tracked tests.
const fakePanel = `
Currentsession
████████                                          16%used
Resets2am(Europe/Copenhagen)

Currentweek(allmodels)
██████████████████████████████▌                   61%used
ResetsMay1,1am(Europe/Copenhagen)

Currentweek(Sonnetonly)
                                                    0%used
ResetsMay1,1am(Europe/Copenhagen)
`

func TestParseInto_ExtractsBuckets(t *testing.T) {
	res := parseInto(Result{}, fakePanel)
	if len(res.Buckets) != 3 {
		t.Fatalf("want 3 buckets, got %d: %+v", len(res.Buckets), res.Buckets)
	}
	if res.Buckets[0].Pct != 16 {
		t.Errorf("session pct: want 16, got %d", res.Buckets[0].Pct)
	}
	if res.Buckets[1].Pct != 61 {
		t.Errorf("week-all pct: want 61, got %d", res.Buckets[1].Pct)
	}
	if !strings.Contains(strings.ToLower(res.Buckets[0].Label), "session") {
		t.Errorf("first bucket label should mention 'session': %q", res.Buckets[0].Label)
	}
	if res.Buckets[0].ResetRaw != "2am" {
		t.Errorf("session reset_raw: want %q, got %q", "2am", res.Buckets[0].ResetRaw)
	}
	if res.Buckets[1].ResetRaw != "May1,1am" {
		t.Errorf("week reset_raw: want %q, got %q", "May1,1am", res.Buckets[1].ResetRaw)
	}
}

func TestParseInto_PicksSessionAndWeek(t *testing.T) {
	res := parseInto(Result{}, fakePanel)
	if res.SessionPct == nil || *res.SessionPct != 16 {
		t.Errorf("SessionPct: want 16, got %v", res.SessionPct)
	}
	if res.WeekAllPct == nil || *res.WeekAllPct != 61 {
		t.Errorf("WeekAllPct: want 61, got %v", res.WeekAllPct)
	}
}

func TestStripANSI(t *testing.T) {
	in := []byte("\x1b[31mhello\x1b[0m\x1b7world\x1b8")
	got := stripANSI(in)
	if got != "helloworld" {
		t.Errorf("stripANSI: want %q, got %q", "helloworld", got)
	}
}

func TestParseReset_TimeOnly_NearestFuture(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	got, ok := ParseReset("2am", now)
	if !ok {
		t.Fatal("ParseReset(2am) returned ok=false")
	}
	want := time.Date(2026, 5, 2, 2, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("ParseReset(2am): want %s, got %s", want, got)
	}
}

func TestParseReset_DateAndTime(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	got, ok := ParseReset("May1,1am", now)
	if !ok {
		t.Fatal("ParseReset(May1,1am) returned ok=false")
	}
	// "May 1, 1am" but joined.
	want := time.Date(2026, 5, 1, 1, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("ParseReset(May1,1am): want %s, got %s", want, got)
	}
}

func TestParseReset_PastBecomesNextOccurrence(t *testing.T) {
	now := time.Date(2026, 4, 28, 14, 0, 0, 0, time.UTC) // 2pm
	got, ok := ParseReset("1am", now)
	if !ok {
		t.Fatal("ParseReset(1am) returned ok=false")
	}
	// Should roll to tomorrow.
	want := time.Date(2026, 4, 29, 1, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("ParseReset(1am at 2pm): want %s, got %s", want, got)
	}
}

func TestParseReset_Empty(t *testing.T) {
	if _, ok := ParseReset("", time.Now()); ok {
		t.Error("ParseReset(\"\"): want ok=false")
	}
}

func TestParseReset_Garbage(t *testing.T) {
	if _, ok := ParseReset("not a time", time.Now()); ok {
		t.Error("ParseReset garbage: want ok=false")
	}
}
