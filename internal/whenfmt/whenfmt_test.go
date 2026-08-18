package whenfmt

import (
	"testing"
	"time"
)

// TestDeadline pins the whole countdown-versus-weekday decision. Every
// case is anchored to a fixed instant in a fixed zone, because two of the
// rules under test (which weekday a reset lands on, and how many calendar
// days ahead that is) are answers about a local clock and would otherwise
// depend on the machine running the test.
//
// The anchor is Wednesday 2026-08-05 08:00 at UTC+01:00.
func TestDeadline(t *testing.T) {
	zone := time.FixedZone("test", 3600)
	now := time.Date(2026, 8, 5, 8, 0, 0, 0, zone)
	at := func(day, hour, min int) time.Time {
		return time.Date(2026, 8, day, hour, min, 0, 0, zone)
	}

	cases := []struct {
		name string
		at   time.Time
		want string
	}{
		// Under a day: always a countdown, whatever the clock says.
		{"minutes away", at(5, 8, 33), "33m"},
		{"hours away", at(5, 23, 12), "15h12m"},
		{"just under the 24h cutoff", at(6, 7, 59), "23h59m"},

		// Past it, and outside the grace band: named day plus its time.
		{"exactly at the 24h cutoff", at(6, 8, 0), "thu 08:00"},
		{"midday two days out", at(7, 15, 30), "fri 15:30"},
		{"six days out is still unique", at(11, 15, 30), "tue 15:30"},

		// Inside the grace band either side of midnight: bare day name,
		// and both halves of the band must agree on which day that is.
		{"exactly midnight", at(7, 0, 0), "fri"},
		{"just after midnight", at(7, 2, 59), "fri"},
		{"at the after-midnight edge", at(7, 3, 0), "fri"},
		{"just past the after-midnight edge", at(7, 3, 1), "fri 03:01"},
		{"just before midnight rounds up to the next day", at(6, 22, 0), "fri"},
		{"at the before-midnight edge", at(6, 21, 0), "fri"},
		{"just past the before-midnight edge", at(6, 20, 59), "thu 20:59"},

		// Too far ahead for a weekday name to be unique: a Wednesday
		// seven days out would print "wed" and read as today.
		{"seven days out falls back to a countdown", at(12, 15, 30), "7d7h"},
		{"rounding up past the bound also falls back", at(11, 23, 0), "6d15h"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Deadline(tc.at, now); got != tc.want {
				t.Errorf("Deadline(%s) = %q, want %q", tc.at.Format(time.RFC3339), got, tc.want)
			}
		})
	}
}

// TestDeadline_UsesCallerZone proves the rendering is computed on the
// reader's clock rather than in UTC, and it is the same instant that
// changes answer, not two different ones.
//
// Friday 20:00 UTC sits four hours before Saturday midnight, outside the
// grace band, so in UTC it renders with its time of day. Two hours east
// the very same instant is Friday 22:00 local, two hours before midnight
// and therefore inside the band, which rounds it up to a bare "sat".
// Reading the timestamp in the wrong zone is off by a day here, which is
// why Deadline takes its zone from now rather than from the parsed
// (always-UTC) reset timestamp.
func TestDeadline_UsesCallerZone(t *testing.T) {
	at := time.Date(2026, 8, 7, 20, 0, 0, 0, time.UTC)

	utcNow := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
	if got, want := Deadline(at, utcNow), "fri 20:00"; got != want {
		t.Errorf("in UTC: Deadline = %q, want %q", got, want)
	}

	cestNow := utcNow.In(time.FixedZone("CEST", 2*3600))
	if got, want := Deadline(at, cestNow), "sat"; got != want {
		t.Errorf("at UTC+2: Deadline = %q, want %q", got, want)
	}
}

// TestDaysBetween_AcrossDST guards the one arithmetic subtlety in the
// weekday bound: across a DST transition a calendar day is 23 or 25 hours
// long, so counting days by dividing a duration by 24 and truncating puts
// the answer one day out twice a year — which would silently move the
// maxNamedDayOffset cutoff. Europe/Copenhagen springs forward on
// 2026-03-29 and falls back on 2026-10-25.
func TestDaysBetween_AcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Copenhagen")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	cases := []struct {
		name     string
		from, to time.Time
		want     int
	}{
		{
			name: "spring forward: a 23-hour day still counts as one",
			from: time.Date(2026, 3, 28, 12, 0, 0, 0, loc),
			to:   time.Date(2026, 3, 29, 12, 0, 0, 0, loc),
			want: 1,
		},
		{
			name: "fall back: a 25-hour day still counts as one",
			from: time.Date(2026, 10, 24, 12, 0, 0, 0, loc),
			to:   time.Date(2026, 10, 25, 12, 0, 0, 0, loc),
			want: 1,
		},
		{
			name: "a full week spanning a transition",
			from: time.Date(2026, 3, 26, 12, 0, 0, 0, loc),
			to:   time.Date(2026, 4, 2, 12, 0, 0, 0, loc),
			want: 7,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := daysBetween(tc.from, tc.to); got != tc.want {
				t.Errorf("daysBetween = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestPhrase covers the preposition, which is the only thing Phrase adds and
// the only thing that can be wrong: a countdown takes "in", a named day takes
// "on", and reading it off the string shape rather than off the branch would
// break the first time a weekday name starts with a digit-like glyph.
func TestPhrase(t *testing.T) {
	zone := time.FixedZone("test", 3600)
	now := time.Date(2026, 8, 5, 8, 0, 0, 0, zone)

	cases := []struct {
		name string
		at   time.Time
		want string
	}{
		{"a countdown", time.Date(2026, 8, 5, 12, 30, 0, 0, zone), "in 4h30m"},
		{"a named day", time.Date(2026, 8, 7, 15, 30, 0, 0, zone), "on fri 15:30"},
		{"a bare day", time.Date(2026, 8, 8, 0, 0, 0, 0, zone), "on sat"},
		{"too far out to name", time.Date(2026, 8, 12, 8, 0, 0, 0, zone), "in 7d"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Phrase(c.at, now); got != c.want {
				t.Errorf("Phrase = %q, want %q", got, c.want)
			}
		})
	}
}
