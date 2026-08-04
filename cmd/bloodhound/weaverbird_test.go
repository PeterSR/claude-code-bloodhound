package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	wb "github.com/PeterSR/claude-code-weaverbird/provider"

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
	"github.com/PeterSR/claude-code-bloodhound/internal/usage"
)

// TestWeaverbirdSpec_Widgets pins the static contract: six widgets, all
// kind: text (kind: meter is deliberately ruled out for .5h/.week
// specifically), the ids weaverbird's descriptor and
// EXAMPLES.md reference, the bloodhound icon, and the two opt-in widgets'
// static fields (Row, Priority, Cache, and Default marking them opt-in).
// The warn/danger thresholds are not on the spec at all: weaverbird has no
// threshold map. They are pinned instead against classForPercentage in
// TestClassForPercentage. The three default widgets must lead the slice:
// the implicit default group is Widgets in this order, so a caller relying
// on it must see 5h, week and resume first.
func TestWeaverbirdSpec_Widgets(t *testing.T) {
	if weaverbirdSpec.Provider != "bloodhound" {
		t.Errorf("Provider = %q, want bloodhound", weaverbirdSpec.Provider)
	}
	if weaverbirdSpec.Icon != "🩸" {
		t.Errorf("Icon = %q, want the bloodhound icon", weaverbirdSpec.Icon)
	}
	if len(weaverbirdSpec.Widgets) != 6 {
		t.Fatalf("len(Widgets) = %d, want 6", len(weaverbirdSpec.Widgets))
	}
	wantLead := []string{"bloodhound.5h", "bloodhound.week", "bloodhound.resume.5h"}
	for i, id := range wantLead {
		if weaverbirdSpec.Widgets[i].ID != id {
			t.Errorf("Widgets[%d].ID = %q, want %q (default widgets lead the implicit default group)",
				i, weaverbirdSpec.Widgets[i].ID, id)
		}
	}

	byID := map[string]wb.Widget{}
	for _, w := range weaverbirdSpec.Widgets {
		byID[w.ID] = w
	}

	fiveH, ok := byID["bloodhound.5h"]
	if !ok {
		t.Fatal("missing bloodhound.5h widget")
	}
	if fiveH.Title != "5 hour quota" || fiveH.Priority != 10 {
		t.Errorf("bloodhound.5h = %+v, want title %q priority 10", fiveH, "5 hour quota")
	}
	if fiveH.Kind != wb.KindText {
		t.Errorf("bloodhound.5h.Kind = %q, want %q", fiveH.Kind, wb.KindText)
	}
	if fiveH.Cache == nil || fiveH.Cache.TTLSec != 10 {
		t.Errorf("bloodhound.5h.Cache = %+v, want ttl_sec=10", fiveH.Cache)
	}
	if !fiveH.IsDefault() {
		t.Error("bloodhound.5h.IsDefault() = false, want true (unchanged default widget)")
	}

	week, ok := byID["bloodhound.week"]
	if !ok {
		t.Fatal("missing bloodhound.week widget")
	}
	if week.Title != "Weekly quota" || week.Priority != 20 {
		t.Errorf("bloodhound.week = %+v, want title %q priority 20", week, "Weekly quota")
	}
	if week.Kind != wb.KindText {
		t.Errorf("bloodhound.week.Kind = %q, want %q", week.Kind, wb.KindText)
	}
	if week.Cache == nil || week.Cache.TTLSec != 10 {
		t.Errorf("bloodhound.week.Cache = %+v, want ttl_sec=10", week.Cache)
	}
	if !week.IsDefault() {
		t.Error("bloodhound.week.IsDefault() = false, want true (unchanged default widget)")
	}

	resume, ok := byID["bloodhound.resume.5h"]
	if !ok {
		t.Fatal("missing bloodhound.resume.5h widget")
	}
	if resume.Title != "Cold-cache resume cost (5h)" || resume.Priority != 25 {
		t.Errorf("bloodhound.resume.5h = %+v, want title %q priority 25", resume, "Cold-cache resume cost (5h)")
	}
	if resume.Kind != wb.KindText {
		t.Errorf("bloodhound.resume.5h.Kind = %q, want %q", resume.Kind, wb.KindText)
	}
	if resume.Row != 0 {
		t.Errorf("bloodhound.resume.5h.Row = %d, want 0 (top row, beside the gauges it qualifies)", resume.Row)
	}
	if resume.Cache == nil || resume.Cache.TTLSec != 10 {
		t.Errorf("bloodhound.resume.5h.Cache = %+v, want ttl_sec=10", resume.Cache)
	}
	if len(resume.DataDeps) != 1 || resume.DataDeps[0] != "session" {
		t.Errorf("bloodhound.resume.5h.DataDeps = %v, want [session] (it is scoped to the piped session id)", resume.DataDeps)
	}
	if !resume.IsDefault() {
		t.Error("bloodhound.resume.5h.IsDefault() = false, want true (it suppresses itself, so it costs nothing to leave on)")
	}
	// The priority ordering is the whole argument for a self-suppressing
	// widget outranking the always-on gauges; pin the relation, not just
	// the number, so a later reshuffle of the gauges cannot quietly
	// invert it.
	if resume.Priority <= fiveH.Priority || resume.Priority <= week.Priority {
		t.Errorf("bloodhound.resume.5h.Priority = %d, want above both 5h (%d) and week (%d)",
			resume.Priority, fiveH.Priority, week.Priority)
	}

	resumeWk, ok := byID["bloodhound.resume.week"]
	if !ok {
		t.Fatal("missing bloodhound.resume.week widget")
	}
	if resumeWk.Title != "Cold-cache resume cost (weekly)" || resumeWk.Priority != 8 {
		t.Errorf("bloodhound.resume.week = %+v, want title %q priority 8", resumeWk, "Cold-cache resume cost (weekly)")
	}
	if resumeWk.Kind != wb.KindText {
		t.Errorf("bloodhound.resume.week.Kind = %q, want %q", resumeWk.Kind, wb.KindText)
	}
	if resumeWk.IsDefault() {
		t.Error("bloodhound.resume.week.IsDefault() = true, want false (opt-in detail)")
	}
	// The pair is deliberately ranked apart, and the direction is the
	// point: the same cold resume is a large slice of the 5h pool and a
	// small one of the weekly pool, so the weekly reading must be the
	// first of the two dropped under width pressure, not the last.
	if resumeWk.Priority >= resume.Priority {
		t.Errorf("bloodhound.resume.week.Priority = %d, want below its 5h twin (%d)",
			resumeWk.Priority, resume.Priority)
	}
	if resumeWk.Priority >= fiveH.Priority || resumeWk.Priority >= week.Priority {
		t.Errorf("bloodhound.resume.week.Priority = %d, want below both gauges (5h %d, week %d)",
			resumeWk.Priority, fiveH.Priority, week.Priority)
	}

	burn, ok := byID["bloodhound.burn"]
	if !ok {
		t.Fatal("missing bloodhound.burn widget")
	}
	if burn.Priority != 15 || burn.Row != 1 {
		t.Errorf("bloodhound.burn = %+v, want priority 15 row 1", burn)
	}
	if burn.Kind != wb.KindText {
		t.Errorf("bloodhound.burn.Kind = %q, want %q", burn.Kind, wb.KindText)
	}
	if burn.Cache == nil || burn.Cache.TTLSec != 10 {
		t.Errorf("bloodhound.burn.Cache = %+v, want ttl_sec=10", burn.Cache)
	}
	if burn.IsDefault() {
		t.Error("bloodhound.burn.IsDefault() = true, want false (opt-in)")
	}

	poll, ok := byID["bloodhound.poll"]
	if !ok {
		t.Fatal("missing bloodhound.poll widget")
	}
	if poll.Priority != 5 || poll.Row != 1 {
		t.Errorf("bloodhound.poll = %+v, want priority 5 row 1", poll)
	}
	if poll.Kind != wb.KindText {
		t.Errorf("bloodhound.poll.Kind = %q, want %q", poll.Kind, wb.KindText)
	}
	if poll.Cache == nil || poll.Cache.TTLSec != 10 {
		t.Errorf("bloodhound.poll.Cache = %+v, want ttl_sec=10", poll.Cache)
	}
	if poll.IsDefault() {
		t.Error("bloodhound.poll.IsDefault() = true, want false (opt-in)")
	}
}

// TestClassForPercentage pins classForPercentage against the exact
// thresholds bloodhound applies to .5h/.week: danger at or above 90, warn
// at or above 70, else ok. It also pins that a non-empty
// explicit class always wins outright, matching ResolveClass's "explicit
// class on the record wins" rule — the same override bucketValue relies on
// for its saturated/limit-projection cases.
func TestClassForPercentage(t *testing.T) {
	cases := []struct {
		name     string
		explicit string
		pct      int
		want     string
	}{
		{"below warn: ok", "", 38, wb.ClassOK},
		{"at warn threshold: warn", "", 70, wb.ClassWarn},
		{"between warn and danger: warn", "", 85, wb.ClassWarn},
		{"at danger threshold: danger", "", 90, wb.ClassDanger},
		{"above danger: danger", "", 95, wb.ClassDanger},
		{"explicit override below every threshold still wins", wb.ClassDanger, 10, wb.ClassDanger},
		{"explicit override at a percentage the threshold would also call danger", wb.ClassDanger, 95, wb.ClassDanger},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classForPercentage(tc.explicit, tc.pct); got != tc.want {
				t.Errorf("classForPercentage(%q, %d) = %q, want %q", tc.explicit, tc.pct, got, tc.want)
			}
		})
	}
}

// TestWeaverbirdSpec_Groups pins the "bloodhound.detail" group: all six
// widget ids, in the order a caller should render them, under one handle a
// user's layout can reference without spelling out every id.
func TestWeaverbirdSpec_Groups(t *testing.T) {
	if len(weaverbirdSpec.Groups) != 1 {
		t.Fatalf("len(Groups) = %d, want 1", len(weaverbirdSpec.Groups))
	}
	g := weaverbirdSpec.Groups[0]
	if g.ID != "bloodhound.detail" {
		t.Errorf("Groups[0].ID = %q, want bloodhound.detail", g.ID)
	}
	if g.Title != "Quota detail" {
		t.Errorf("Groups[0].Title = %q, want %q", g.Title, "Quota detail")
	}
	want := []string{
		"bloodhound.5h", "bloodhound.week",
		"bloodhound.resume.5h", "bloodhound.resume.week",
		"bloodhound.burn", "bloodhound.poll",
	}
	if len(g.Widgets) != len(want) {
		t.Fatalf("Groups[0].Widgets = %v, want %v", g.Widgets, want)
	}
	for i, id := range want {
		if g.Widgets[i] != id {
			t.Errorf("Groups[0].Widgets[%d] = %q, want %q", i, g.Widgets[i], id)
		}
	}
}

// TestWeaverbirdValue_NoData covers the silent case: a freshly opened
// store with no /usage observation yet must yield no value records at
// all, not a placeholder or a zeroed widget, so weaverbird drops the
// bloodhound section rather than showing a fake 0%.
func TestWeaverbirdValue_NoData(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	vals, err := weaverbirdValue(wb.Session{}, nil)
	if err != nil {
		t.Fatalf("weaverbirdValue: %v", err)
	}
	if len(vals) != 0 {
		t.Errorf("vals = %+v, want none (no observation captured yet)", vals)
	}
}

// TestBucketValue_Classes exercises every class bucketValue can produce,
// now that it computes class itself instead of handing a percentage to a
// host-side `states` map: the plain-percentage case (classForPercentage's
// ok/warn/danger thresholds), the danger threshold already crossed by the
// percentage alone, and the two cases where the percentage would not
// capture the urgency and an explicit danger override applies: saturated,
// and a burn-rate limit projection at a percentage still below the danger
// threshold. Percentage is no longer a field on a kind:text value record
// at all (weaverbird SPEC.md section 4), so this only asserts Class now.
func TestBucketValue_Classes(t *testing.T) {
	cases := []struct {
		name      string
		w         *routes.NowWindow
		wantClass string
	}{
		{
			name:      "below warn threshold: ok",
			w:         &routes.NowWindow{Pct: 38},
			wantClass: wb.ClassOK,
		},
		{
			name:      "past danger threshold via percentage alone: danger",
			w:         &routes.NowWindow{Pct: 95},
			wantClass: wb.ClassDanger,
		},
		{
			name:      "saturated below the danger threshold: percentage alone would not capture it",
			w:         &routes.NowWindow{Pct: 85, Saturated: true},
			wantClass: wb.ClassDanger,
		},
		{
			name:      "saturated at a percentage already past danger: threshold already covers it",
			w:         &routes.NowWindow{Pct: 99, Saturated: true},
			wantClass: wb.ClassDanger,
		},
		{
			name:      "burn-rate limit projected while pct is still low: explicit danger",
			w:         &routes.NowWindow{Pct: 40, LimitOK: true, LimitETAMS: int64(90 * time.Minute / time.Millisecond)},
			wantClass: wb.ClassDanger,
		},
		{
			name:      "limit projected but pct already past danger threshold: threshold already covers it",
			w:         &routes.NowWindow{Pct: 92, LimitOK: true, LimitETAMS: int64(time.Hour / time.Millisecond)},
			wantClass: wb.ClassDanger,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := bucketValue("bloodhound.5h", "5h", tc.w, time.Now())
			if v.ID != "bloodhound.5h" {
				t.Errorf("ID = %q, want bloodhound.5h", v.ID)
			}
			if v.Class != tc.wantClass {
				t.Errorf("Class = %q, want %q", v.Class, tc.wantClass)
			}
		})
	}
}

// TestBucketValue_Parenthetical pins every shape the parenthetical can
// take, and in particular the property the two-mark form exists for: a
// window under pressure must still report its reset countdown. Before the
// marks, a saturated or limit-projecting window suppressed the reset
// entirely, which lost the answer to "how long until this clears?" at
// exactly the moment the question gets asked.
//
// ShortText stays the bare percentage in every case: it is what weaverbird
// falls back to under width pressure, so it must not grow when the marks
// do.
//
// Cases without a ResetTSISO exercise fmtReset's countdown fallback (the
// /usage panel gave nothing parsable to name); the two at the end carry
// one, and are what proves the reset mark reaches fmtDeadline's absolute
// branch while the limit mark beside it stays a countdown.
func TestBucketValue_Parenthetical(t *testing.T) {
	const (
		min33 = int64(33 * time.Minute / time.Millisecond)
		h3m12 = int64((3*time.Hour + 12*time.Minute) / time.Millisecond)
		d2h7  = int64((2*24*time.Hour + 7*time.Hour) / time.Millisecond)
	)
	// Wednesday 08:00 in a fixed +01:00 zone, so the weekday arithmetic
	// below cannot depend on the machine running the test.
	now := time.Date(2026, 8, 5, 8, 0, 0, 0, time.FixedZone("test", 3600))

	cases := []struct {
		name string
		w    *routes.NowWindow
		want string
	}{
		{
			name: "reset only",
			w:    &routes.NowWindow{Pct: 72, TimeToResetMS: h3m12},
			want: "5h 72% (↻3h12m)",
		},
		{
			name: "limit projection keeps the reset countdown beside it",
			w:    &routes.NowWindow{Pct: 72, LimitOK: true, LimitETAMS: min33, TimeToResetMS: h3m12},
			want: "5h 72% (⚠ 33m ↻3h12m)",
		},
		{
			name: "saturated keeps the reset countdown beside it",
			w:    &routes.NowWindow{Pct: 99, Saturated: true, TimeToResetMS: h3m12},
			want: "5h 99% (⊘ ↻3h12m)",
		},
		{
			name: "saturated outranks a stale limit projection",
			w:    &routes.NowWindow{Pct: 99, Saturated: true, LimitOK: true, LimitETAMS: min33, TimeToResetMS: h3m12},
			want: "5h 99% (⊘ ↻3h12m)",
		},
		{
			name: "limit projection with no parsed reset",
			w:    &routes.NowWindow{Pct: 72, LimitOK: true, LimitETAMS: min33},
			want: "5h 72% (⚠ 33m)",
		},
		{
			name: "saturated with no parsed reset",
			w:    &routes.NowWindow{Pct: 99, Saturated: true},
			want: "5h 99% (⊘)",
		},
		{
			name: "nothing to qualify: no parentheses at all",
			w:    &routes.NowWindow{Pct: 12},
			want: "5h 12%",
		},
		{
			// Friday 15:30 local, two-and-a-bit days from the fixed now.
			name: "a parsed reset more than a day out is named, not counted",
			w: &routes.NowWindow{
				Pct: 39, TimeToResetMS: d2h7,
				ResetTSISO: "2026-08-07T14:30:00Z",
			},
			want: "5h 39% (↻fri 15:30)",
		},
		{
			// The same reset, with a limit projection beside it: the
			// reset goes absolute, the projection stays a countdown.
			name: "the limit mark stays a countdown even when the reset goes absolute",
			w: &routes.NowWindow{
				Pct: 39, LimitOK: true, LimitETAMS: min33, TimeToResetMS: d2h7,
				ResetTSISO: "2026-08-07T14:30:00Z",
			},
			want: "5h 39% (⚠ 33m ↻fri 15:30)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := bucketValue("bloodhound.5h", "5h", tc.w, now)
			if v.FullText != tc.want {
				t.Errorf("FullText = %q, want %q", v.FullText, tc.want)
			}
			wantShort := fmt.Sprintf("%d%%", tc.w.Pct)
			if v.ShortText != wantShort {
				t.Errorf("ShortText = %q, want %q (must not grow with the marks)", v.ShortText, wantShort)
			}
		})
	}
}

// TestGlyphs_SingleRune guards the one property the parenthetical's layout
// depends on and that a careless edit silently breaks: each mark is
// exactly one rune. It does not prove single *display width* — no Go test
// can, that is the terminal font's business — but a multi-rune mark
// (U+26A0 plus a variation selector, say) is the usual way an "invisible"
// second cell sneaks in, and that this test can catch.
func TestGlyphs_SingleRune(t *testing.T) {
	for name, g := range map[string]string{
		"glyphLimit":     glyphLimit,
		"glyphSaturated": glyphSaturated,
		"glyphReset":     glyphReset,
	} {
		if n := utf8.RuneCountInString(g); n != 1 {
			t.Errorf("%s = %q is %d runes, want exactly 1", name, g, n)
		}
	}
}

// TestBurnValue covers every branch of the opt-in "bloodhound.burn"
// widget: no window at all, a projected breach far enough out to stay
// "warn", one close enough to upgrade to "danger" (and the boundary right
// at the 30-minute cutoff, which must still read as "danger" since the
// check is <=), the plain current-rate case when there's no projected
// breach, and the fully-silent case when nowstate couldn't measure
// anything at all.
func TestBurnValue(t *testing.T) {
	if v := burnValue(nil); v != nil {
		t.Errorf("burnValue(nil) = %+v, want nil", v)
	}

	t.Run("limit projected far out: warn", func(t *testing.T) {
		w := &routes.NowWindow{LimitOK: true, LimitETAMS: int64(2 * time.Hour / time.Millisecond)}
		v := burnValue(w)
		if v == nil {
			t.Fatal("burnValue = nil, want a value")
		}
		if v.ID != "bloodhound.burn" {
			t.Errorf("ID = %q, want bloodhound.burn", v.ID)
		}
		if v.Class != wb.ClassWarn {
			t.Errorf("Class = %q, want %q", v.Class, wb.ClassWarn)
		}
		if v.FullText != "burn 5h -> 100% 2h" {
			t.Errorf("Text = %q, want %q", v.FullText, "burn 5h -> 100% 2h")
		}
		if v.ShortText != "2h" {
			t.Errorf("Short = %q, want %q", v.ShortText, "2h")
		}
	})

	t.Run("limit projected right at the 30m cutoff: danger", func(t *testing.T) {
		w := &routes.NowWindow{LimitOK: true, LimitETAMS: 30 * 60 * 1000}
		v := burnValue(w)
		if v == nil || v.Class != wb.ClassDanger {
			t.Errorf("burnValue = %+v, want Class %q", v, wb.ClassDanger)
		}
	})

	t.Run("limit projected just past the cutoff: warn", func(t *testing.T) {
		w := &routes.NowWindow{LimitOK: true, LimitETAMS: 30*60*1000 + 1}
		v := burnValue(w)
		if v == nil || v.Class != wb.ClassWarn {
			t.Errorf("burnValue = %+v, want Class %q", v, wb.ClassWarn)
		}
	})

	t.Run("no breach projected, whole-number rate: neutral", func(t *testing.T) {
		w := &routes.NowWindow{BurnOK: true, BurnPctPerHour: 12}
		v := burnValue(w)
		if v == nil {
			t.Fatal("burnValue = nil, want a value")
		}
		if v.Class != wb.ClassNeutral {
			t.Errorf("Class = %q, want %q", v.Class, wb.ClassNeutral)
		}
		if v.FullText != "burn 5h 12%/h" {
			t.Errorf("Text = %q, want %q", v.FullText, "burn 5h 12%/h")
		}
		if v.ShortText != "12%/h" {
			t.Errorf("Short = %q, want %q", v.ShortText, "12%/h")
		}
	})

	t.Run("no breach projected, fractional rate: one decimal", func(t *testing.T) {
		w := &routes.NowWindow{BurnOK: true, BurnPctPerHour: 2.3}
		v := burnValue(w)
		if v == nil || v.FullText != "burn 5h 2.3%/h" {
			t.Errorf("burnValue = %+v, want Text %q", v, "burn 5h 2.3%/h")
		}
	})

	t.Run("nothing measurable: silent", func(t *testing.T) {
		w := &routes.NowWindow{Pct: 12}
		if v := burnValue(w); v != nil {
			t.Errorf("burnValue = %+v, want nil (no BurnOK, no LimitOK)", v)
		}
	})
}

// TestFmtBurnRate pins the compact-formatting rule directly: integral
// slopes (including zero) print with no decimal, anything else keeps one.
func TestFmtBurnRate(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0%/h"},
		{12, "12%/h"},
		{2.3, "2.3%/h"},
		{2.34, "2.3%/h"},
	}
	for _, tc := range cases {
		if got := fmtBurnRate(tc.in); got != tc.want {
			t.Errorf("fmtBurnRate(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestPollValue covers every branch of the opt-in "bloodhound.poll"
// widget: no observation yet (silent, matching weaverbirdValue's own
// no-data behavior), a failed extraction (danger regardless of age), a
// fresh poll under the staleness threshold (neutral), one past it
// (stale), and an unset/unknown threshold (staleAfterS <= 0), which must
// not manufacture false staleness out of a zero value.
func TestPollValue(t *testing.T) {
	if v := pollValue(nil, 600); v != nil {
		t.Errorf("pollValue(nil, ...) = %+v, want nil", v)
	}

	t.Run("extraction failed: danger regardless of age", func(t *testing.T) {
		v := pollValue(&routes.NowPoll{ParseOK: false, AgeS: 3}, 600)
		if v == nil {
			t.Fatal("pollValue = nil, want a value")
		}
		if v.ID != "bloodhound.poll" {
			t.Errorf("ID = %q, want bloodhound.poll", v.ID)
		}
		if v.Class != wb.ClassDanger {
			t.Errorf("Class = %q, want %q", v.Class, wb.ClassDanger)
		}
		if v.FullText != "extraction failed" {
			t.Errorf("Text = %q, want %q", v.FullText, "extraction failed")
		}
	})

	t.Run("fresh: neutral", func(t *testing.T) {
		v := pollValue(&routes.NowPoll{ParseOK: true, AgeS: 45}, 600)
		if v == nil {
			t.Fatal("pollValue = nil, want a value")
		}
		if v.Class != wb.ClassNeutral {
			t.Errorf("Class = %q, want %q", v.Class, wb.ClassNeutral)
		}
		if v.FullText != "poll 45s" {
			t.Errorf("Text = %q, want %q", v.FullText, "poll 45s")
		}
		if v.ShortText != "45s" {
			t.Errorf("Short = %q, want %q", v.ShortText, "45s")
		}
	})

	t.Run("past the staleness threshold: stale", func(t *testing.T) {
		v := pollValue(&routes.NowPoll{ParseOK: true, AgeS: 700}, 600)
		if v == nil {
			t.Fatal("pollValue = nil, want a value")
		}
		if v.Class != wb.ClassStale {
			t.Errorf("Class = %q, want %q", v.Class, wb.ClassStale)
		}
		if v.FullText != "poll stale 11m" {
			t.Errorf("Text = %q, want %q", v.FullText, "poll stale 11m")
		}
	})

	t.Run("unset staleness threshold: never manufactured stale", func(t *testing.T) {
		v := pollValue(&routes.NowPoll{ParseOK: true, AgeS: 100000}, 0)
		if v == nil {
			t.Fatal("pollValue = nil, want a value")
		}
		if v.Class != wb.ClassNeutral {
			t.Errorf("Class = %q, want %q (staleAfterS<=0 must not manufacture staleness)", v.Class, wb.ClassNeutral)
		}
	})
}

// TestWeaverbirdValue_SingleObservation_NoBurnYet exercises the real
// nowstate.Compute path (not the bare helpers above): after exactly one
// /usage observation, bloodhound.5h/week already have data but
// bloodhound.burn cannot yet — slopeOver needs two points — while
// bloodhound.poll can, immediately, off that same single observation.
// Pins that burnValue's silence propagates correctly through
// weaverbirdValue rather than only being tested in isolation.
func TestWeaverbirdValue_SingleObservation_NoBurnYet(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	ctx := context.Background()
	s, err := store.Open(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	sessionPct, weekPct := 20, 30
	if _, err := s.RecordUsage(ctx, usage.Result{
		OK:         true,
		FetchedAt:  time.Now(),
		SessionPct: &sessionPct,
		WeekPct:    &weekPct,
	}, nil); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	vals, err := weaverbirdValue(wb.Session{}, nil)
	if err != nil {
		t.Fatalf("weaverbirdValue: %v", err)
	}

	byID := map[string]wb.Value{}
	for _, v := range vals {
		byID[v.ID] = v
	}

	if _, ok := byID["bloodhound.5h"]; !ok {
		t.Error("missing bloodhound.5h value")
	}
	if _, ok := byID["bloodhound.week"]; !ok {
		t.Error("missing bloodhound.week value")
	}
	if v, ok := byID["bloodhound.burn"]; ok {
		t.Errorf("bloodhound.burn = %+v, want absent (only one observation, no slope yet)", v)
	}
	poll, ok := byID["bloodhound.poll"]
	if !ok {
		t.Fatal("missing bloodhound.poll value")
	}
	if poll.Class != wb.ClassNeutral {
		t.Errorf("bloodhound.poll.Class = %q, want %q (just polled)", poll.Class, wb.ClassNeutral)
	}
}

// TestWeaverbirdValue_TwoObservations_BurnAppears extends the above to two
// observations 30 minutes apart with a rising session percentage: enough
// for nowstate's slopeOver to measure a rate, so bloodhound.burn must now
// appear. The rate (30%/h) projects a breach well beyond both the window's
// unset reset and the 30-minute danger cutoff, so LimitOK end up governing
// the branch (not BurnOK) and the class is "warn".
func TestWeaverbirdValue_TwoObservations_BurnAppears(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	ctx := context.Background()
	s, err := store.Open(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	first, second := 20, 35
	now := time.Now()
	if _, err := s.RecordUsage(ctx, usage.Result{
		OK:         true,
		FetchedAt:  now.Add(-30 * time.Minute),
		SessionPct: &first,
	}, nil); err != nil {
		t.Fatalf("RecordUsage (first): %v", err)
	}
	if _, err := s.RecordUsage(ctx, usage.Result{
		OK:         true,
		FetchedAt:  now,
		SessionPct: &second,
	}, nil); err != nil {
		t.Fatalf("RecordUsage (second): %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	vals, err := weaverbirdValue(wb.Session{}, nil)
	if err != nil {
		t.Fatalf("weaverbirdValue: %v", err)
	}

	byID := map[string]wb.Value{}
	for _, v := range vals {
		byID[v.ID] = v
	}

	burn, ok := byID["bloodhound.burn"]
	if !ok {
		t.Fatal("missing bloodhound.burn value (two points, 15pp/30m should yield a measurable slope)")
	}
	if burn.Class != wb.ClassWarn {
		t.Errorf("bloodhound.burn.Class = %q, want %q; got %+v", burn.Class, wb.ClassWarn, burn)
	}
	if !strings.HasPrefix(burn.FullText, "burn 5h -> 100% ") {
		t.Errorf("bloodhound.burn.FullText = %q, want prefix %q", burn.FullText, "burn 5h -> 100% ")
	}
}

// TestStatusCommand_OutputUnchanged pins the existing `bloodhound status`
// behavior on an empty store. Adding the weaverbird subcommand must not
// touch statusCmd or internal/statusline at all; this regression test
// exists to catch it if it ever does. It does not feed any session JSON
// on stdin (readSessionUUIDFromStdin degrades to "" on anything but a
// piped payload), which is fine here: an empty store prints "no data"
// regardless of which session it was asked about.
func TestStatusCommand_OutputUnchanged(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	if err := statusCmd.RunE(cmd, nil); err != nil {
		t.Fatalf("status RunE: %v", err)
	}

	got := strings.TrimSpace(buf.String())
	const want = "🩸 no data"
	if got != want {
		t.Errorf("status output = %q, want %q (unchanged from before the weaverbird subcommand)", got, want)
	}
}

// TestStatusCommand_OutputUnchanged_WithObservation extends the empty-store
// regression above to the common case: a store that already has a captured
// /usage observation. The empty-store test alone never reaches formatBucket,
// so it cannot catch a regression in the formatted percentage line itself;
// this pins those exact bytes too. Seeds via Store.RecordUsage, the same
// path the daemon uses to persist a real scrape, following
// TestBuildSessionJSON_KnownSessionWithAttribution's pattern in
// session_test.go of opening a store on scratch XDG dirs and writing
// through the real store API rather than raw SQL.
func TestStatusCommand_OutputUnchanged_WithObservation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	ctx := context.Background()
	s, err := store.Open(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	sessionPct, weekPct := 42, 68
	if _, err := s.RecordUsage(ctx, usage.Result{
		OK:         true,
		FetchedAt:  time.Now(),
		SessionPct: &sessionPct,
		WeekPct:    &weekPct,
	}, nil); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	if err := statusCmd.RunE(cmd, nil); err != nil {
		t.Fatalf("status RunE: %v", err)
	}

	got := strings.TrimSpace(buf.String())
	const want = "🩸 42%/5h · 68%/wk"
	if got != want {
		t.Errorf("status output = %q, want %q (unchanged from before the weaverbird subcommand)", got, want)
	}
}

// TestWeaverbirdValue_FastOnEmptyStore is a sanity check that the value
// path stays well under weaverbird's ~500ms render budget on an empty
// store, so a cold bloodhound section can never be the thing that makes
// the whole bar miss its debounce window.
func TestWeaverbirdValue_FastOnEmptyStore(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	start := time.Now()
	if _, err := weaverbirdValue(wb.Session{}, nil); err != nil {
		t.Fatalf("weaverbirdValue: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("weaverbirdValue took %s, want under weaverbird's 500ms render budget", elapsed)
	}
}

// TestFmtCostPct pins the compact cost rendering: no figure at all when
// there is nothing to price, "+<1%" for a real but sub-percent cost (never
// "+0%", which reads as free), and a rounded whole number otherwise. The
// leading "+" is part of the contract, not decoration — see fmtCostPct's
// own comment for why "resume 4%" beside "5h 72%" is actively misleading.
func TestFmtCostPct(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, ""},
		{-1, ""},
		{0.4, "+<1%"},
		{0.99, "+<1%"},
		{1, "+1%"},
		{4.2, "+4%"},
		{4.6, "+5%"},
		{12, "+12%"},
	}
	for _, tc := range cases {
		if got := fmtCostPct(tc.in); got != tc.want {
			t.Errorf("fmtCostPct(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestClassForResumeCost pins the severity bands, including both
// boundaries (the checks are >=, so a cost sitting exactly on a threshold
// takes the heavier class) and the deliberate choice of info rather than
// ok at the low end.
func TestClassForResumeCost(t *testing.T) {
	cases := []struct {
		name string
		pct  float64
		want string
	}{
		{"trivial", 0.5, wb.ClassInfo},
		{"just under the warn threshold", 2.9, wb.ClassInfo},
		{"exactly at the warn threshold", resumeWarnPct, wb.ClassWarn},
		{"between warn and danger", 7, wb.ClassWarn},
		{"exactly at the danger threshold", resumeDangerPct, wb.ClassDanger},
		{"well past danger", 40, wb.ClassDanger},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classForResumeCost(tc.pct); got != tc.want {
				t.Errorf("classForResumeCost(%v) = %q, want %q", tc.pct, got, tc.want)
			}
		})
	}
}

// TestWants pins the "no ids means everything" reading of weaverbird's id
// hint, which is what keeps `bloodhound weaverbird value` run by hand from
// silently returning less than the host would get.
func TestWants(t *testing.T) {
	if !wants(nil, "bloodhound.resume.5h") {
		t.Error("wants(nil, ...) = false, want true (no ids means every widget)")
	}
	if !wants([]string{}, "bloodhound.resume.5h") {
		t.Error("wants([], ...) = false, want true (no ids means every widget)")
	}
	if !wants([]string{"bloodhound.5h", "bloodhound.resume.5h"}, "bloodhound.resume.5h") {
		t.Error("wants(...) = false for an id that was explicitly requested")
	}
	if wants([]string{"bloodhound.5h", "bloodhound.week"}, "bloodhound.resume.5h") {
		t.Error("wants(...) = true for an id the host did not ask for")
	}
}

// insertResumeFixture writes one session and one turn for it, priced so
// the turn's prefix is worth a known number of cost-weighted tokens, and
// aged ageS seconds into the past. cacheCreate1h drives which TTL
// sessioninsight infers, and so whether "cold" is measured against 5
// minutes or an hour.
func insertResumeFixture(t *testing.T, s *store.Store, uuid string, now time.Time, ageS int64, cacheCreate1h bool) {
	t.Helper()
	lastMS := now.UnixMilli() - ageS*1000

	var cc5m, cc1h int64 = 40000, 0
	if cacheCreate1h {
		cc5m, cc1h = 0, 40000
	}

	if _, err := s.DB.Exec(`
		INSERT INTO sessions (
			session_uuid, project, first_ts_unix_ms, last_ts_unix_ms,
			turn_count, raw_tokens, output_tokens, peak_5h_raw_tokens,
			idle_miss_count, rotation_count, restructure_count,
			compaction_count, cold_compaction_count, cache_ttl, models,
			parent_session_uuid, cwd
		) VALUES (?, 'myapp', ?, ?, 1, 50000, 2000, 50000, 0, 0, 0, 0, 0, ?, '', '', '')
	`, uuid, lastMS, lastMS, map[bool]string{true: "1h", false: "5m"}[cacheCreate1h]); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	if _, err := s.DB.Exec(`
		INSERT INTO turns (
			session_uuid, turn_idx, ts, ts_unix_ms, model,
			input_tokens, output_tokens, cache_read,
			cache_create_5m, cache_create_1h,
			gap_s, classification, post_compact, project, source_path_hash
		) VALUES (?, 0, ?, ?, 'claude-opus-4-1', 1200, 2000, 8000, ?, ?, 0, 'normal', 0, 'myapp', '')
	`, uuid, time.UnixMilli(lastMS).UTC().Format(time.RFC3339), lastMS, cc5m, cc1h); err != nil {
		t.Fatalf("insert turn: %v", err)
	}
}

// TestResumeValue covers the widget's whole decision tree against a real
// store: silent with no session id, silent for an id bloodhound has not
// ingested, silent while the cache is still warm, and a priced record once
// the session's age has actually crossed the TTL its last turn cached at.
// The 5m/1h pair matters because they cross at wildly different ages —
// a session idle for ten minutes is cold on a 5m TTL and comfortably warm
// on a 1h one, and the widget must not guess.
func TestResumeValue(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	ctx := context.Background()
	s, err := store.Open(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	now := time.Now()
	insertResumeFixture(t, s, "session-cold5m", now, 900, false) // 15m idle, 5m TTL: cold
	insertResumeFixture(t, s, "session-warm5m", now, 60, false)  // 1m idle, 5m TTL: warm
	insertResumeFixture(t, s, "session-warm1h", now, 900, true)  // 15m idle, 1h TTL: still warm
	insertResumeFixture(t, s, "session-cold1h", now, 7200, true) // 2h idle, 1h TTL: cold

	t.Run("no session id piped in: silent", func(t *testing.T) {
		if v := resumeValues(ctx, s, "", now); len(v) != 0 {
			t.Errorf("resumeValues = %+v, want none", v)
		}
	})

	t.Run("session not ingested yet: silent, never falls back to another session", func(t *testing.T) {
		if v := resumeValues(ctx, s, "session-notingested", now); len(v) != 0 {
			t.Errorf("resumeValues = %+v, want none (a cold sibling session must not be quoted here)", v)
		}
	})

	t.Run("cache still warm on a 5m TTL: silent", func(t *testing.T) {
		if v := resumeValues(ctx, s, "session-warm5m", now); len(v) != 0 {
			t.Errorf("resumeValues = %+v, want none", v)
		}
	})

	t.Run("15m idle on a 1h TTL is still warm: silent", func(t *testing.T) {
		if v := resumeValues(ctx, s, "session-warm1h", now); len(v) != 0 {
			t.Errorf("resumeValues = %+v, want none (the 5m TTL's cutoff must not be applied to a 1h session)", v)
		}
	})

	// No calibration rows exist in this store, so the cost cannot be
	// converted to a percentage for either pool. The 5h record must still
	// fire (knowing the cache is cold is actionable on its own) and the
	// weekly one must not (unpriced, it would only repeat what its twin
	// already said).
	t.Run("cold with no calibration: only the 5h record, and unpriced", func(t *testing.T) {
		vals := resumeValues(ctx, s, "session-cold5m", now)
		if len(vals) != 1 {
			t.Fatalf("resumeValues = %+v, want exactly the 5h record", vals)
		}
		v := vals[0]
		if v.ID != "bloodhound.resume.5h" {
			t.Errorf("ID = %q, want bloodhound.resume.5h", v.ID)
		}
		if v.FullText != "5h resume cold" || v.ShortText != "5h cold" {
			t.Errorf("text = (%q, %q), want (%q, %q)", v.FullText, v.ShortText, "5h resume cold", "5h cold")
		}
		if v.Class != wb.ClassInfo {
			t.Errorf("Class = %q, want %q", v.Class, wb.ClassInfo)
		}
	})

	t.Run("2h idle on a 1h TTL is cold too", func(t *testing.T) {
		if v := resumeValues(ctx, s, "session-cold1h", now); len(v) == 0 {
			t.Error("resumeValues = none, want at least the 5h record")
		}
	})
}

// TestResumeValues_Priced exercises the branch the previous test cannot:
// with a calibration median on file for each pool, the one cold-resume
// cost converts into two percentages and both records fire.
//
// The two medians are deliberately an order of magnitude apart, which is
// the real relationship between the pools (a weekly window holds roughly
// ten times what a 5-hour one does). That is what makes the pair worth
// having: the same cost is danger against one pool and info against the
// other, and a single widget could not carry both classes.
func TestResumeValues_Priced(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	ctx := context.Background()
	s, err := store.Open(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	now := time.Now()
	insertResumeFixture(t, s, "session-cold5m", now, 900, false)

	// One calibration point per bucket is enough for
	// LatestCalibrationMedian to report a median. The exact tokens-per-1%
	// values are chosen only so the fixture's prefix lands in a known band
	// for each pool; the conversion itself is sessioninsight's, already
	// covered by that package's own tests.
	insertCalibrationPoint(t, s, "session", now, 5000)
	insertCalibrationPoint(t, s, "week", now, 50000)

	vals := resumeValues(ctx, s, "session-cold5m", now)
	if len(vals) != 2 {
		t.Fatalf("resumeValues = %+v, want both the 5h and weekly records", vals)
	}

	byID := map[string]wb.Value{}
	for _, v := range vals {
		byID[v.ID] = v
	}

	five, ok := byID["bloodhound.resume.5h"]
	if !ok {
		t.Fatal("missing bloodhound.resume.5h record")
	}
	if !strings.HasPrefix(five.FullText, "5h resume +") {
		t.Errorf("5h FullText = %q, want a %q-prefixed priced form", five.FullText, "5h resume +")
	}
	if five.ShortText != "5h"+strings.TrimPrefix(five.FullText, "5h resume ") {
		t.Errorf("5h ShortText = %q, inconsistent with %q", five.ShortText, five.FullText)
	}
	// ~51k cost-weighted tokens against 5000 per 1% lands around 13%,
	// comfortably past resumeDangerPct under any plausible model
	// multiplier. The exact figure is deliberately not pinned: the
	// multipliers come from a price table loaded at runtime, so an exact
	// string here would fail on a price update that broke nothing.
	if five.Class != wb.ClassDanger {
		t.Errorf("5h Class = %q for %q, want %q", five.Class, five.FullText, wb.ClassDanger)
	}

	wk, ok := byID["bloodhound.resume.week"]
	if !ok {
		t.Fatal("missing bloodhound.resume.week record")
	}
	if !strings.HasPrefix(wk.FullText, "wk resume +") {
		t.Errorf("week FullText = %q, want a %q-prefixed priced form", wk.FullText, "wk resume +")
	}
	if wk.ShortText != "wk"+strings.TrimPrefix(wk.FullText, "wk resume ") {
		t.Errorf("week ShortText = %q, inconsistent with %q", wk.ShortText, wk.FullText)
	}
	// The same cost against a pool ten times larger is about 1.3%, which
	// is below resumeWarnPct. Pinning that the pair disagrees on class is
	// the point of the test — it is the whole justification for two
	// widgets rather than one.
	if wk.Class != wb.ClassInfo {
		t.Errorf("week Class = %q for %q, want %q", wk.Class, wk.FullText, wb.ClassInfo)
	}
	if wk.Class == five.Class {
		t.Error("both records took the same class; the split into two widgets buys nothing if they cannot disagree")
	}
}

// TestResumeValues_NoWeeklyCalibration pins the asymmetric fallback: the
// 5h record still fires unpriced, because "the cache is cold" is worth
// saying on its own, while the weekly record stays silent rather than
// repeating it in a second widget.
func TestResumeValues_NoWeeklyCalibration(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	ctx := context.Background()
	s, err := store.Open(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	now := time.Now()
	insertResumeFixture(t, s, "session-cold5m", now, 900, false)
	insertCalibrationPoint(t, s, "session", now, 5000) // no "week" row

	vals := resumeValues(ctx, s, "session-cold5m", now)
	if len(vals) != 1 {
		t.Fatalf("resumeValues = %+v, want only the 5h record", vals)
	}
	if vals[0].ID != "bloodhound.resume.5h" {
		t.Errorf("ID = %q, want bloodhound.resume.5h", vals[0].ID)
	}
	if !strings.HasPrefix(vals[0].FullText, "5h resume +") {
		t.Errorf("FullText = %q, want the priced 5h form", vals[0].FullText)
	}
}

// insertCalibrationPoint writes one calibration point for a bucket with an
// explicit cost-weighted tokens-per-1%, which is the only field the resume
// widgets read back out.
func insertCalibrationPoint(t *testing.T, s *store.Store, bucket string, now time.Time, tokensPerPctCW float64) {
	t.Helper()
	if _, err := s.DB.Exec(`
		INSERT INTO calibration_points (
			bucket, a_obs_id, b_obs_id, a_ts_unix_ms, b_ts_unix_ms,
			a_pct, b_pct, delta_pct, raw_tokens, cost_weighted_tokens,
			output_tokens, turn_count, gap_s,
			tokens_per_pct_raw, tokens_per_pct_cw
		) VALUES (?, 1, 2, ?, ?, 10, 20, 10, 100000, 50000, 2000, 4, 60, 10000, ?)
	`, bucket, now.UnixMilli()-3600_000, now.UnixMilli(), tokensPerPctCW); err != nil {
		t.Fatalf("insert %s calibration point: %v", bucket, err)
	}
}

// TestFmtDeadline pins the whole countdown-versus-weekday decision. Every
// case is anchored to a fixed instant in a fixed zone, because two of the
// rules under test (which weekday a reset lands on, and how many calendar
// days ahead that is) are answers about a local clock and would otherwise
// depend on the machine running the test.
//
// The anchor is Wednesday 2026-08-05 08:00 at UTC+01:00.
func TestFmtDeadline(t *testing.T) {
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
			if got := fmtDeadline(tc.at, now); got != tc.want {
				t.Errorf("fmtDeadline(%s) = %q, want %q", tc.at.Format(time.RFC3339), got, tc.want)
			}
		})
	}
}

// TestFmtDeadline_UsesCallerZone proves the rendering is computed on the
// reader's clock rather than in UTC, and it is the same instant that
// changes answer, not two different ones.
//
// Friday 20:00 UTC sits four hours before Saturday midnight, outside the
// grace band, so in UTC it renders with its time of day. Two hours east
// the very same instant is Friday 22:00 local, two hours before midnight
// and therefore inside the band, which rounds it up to a bare "sat".
// Reading the timestamp in the wrong zone is off by a day here, which is
// why fmtDeadline takes its zone from now rather than from the parsed
// (always-UTC) reset timestamp.
func TestFmtDeadline_UsesCallerZone(t *testing.T) {
	at := time.Date(2026, 8, 7, 20, 0, 0, 0, time.UTC)

	utcNow := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
	if got, want := fmtDeadline(at, utcNow), "fri 20:00"; got != want {
		t.Errorf("in UTC: fmtDeadline = %q, want %q", got, want)
	}

	cestNow := utcNow.In(time.FixedZone("CEST", 2*3600))
	if got, want := fmtDeadline(at, cestNow), "sat"; got != want {
		t.Errorf("at UTC+2: fmtDeadline = %q, want %q", got, want)
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
