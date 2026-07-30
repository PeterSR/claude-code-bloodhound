package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	wb "github.com/PeterSR/claude-code-weaverbird/provider"

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
	"github.com/PeterSR/claude-code-bloodhound/internal/usage"
)

// TestWeaverbirdSpec_Widgets pins the static contract: four widgets, all
// kind: text (kind: meter is deliberately ruled out for .5h/.week
// specifically), the ids weaverbird's descriptor and
// EXAMPLES.md reference, the bloodhound icon, and the two opt-in widgets'
// static fields (Row, Priority, Cache, and Default marking them opt-in).
// The warn/danger thresholds are not on the spec at all: weaverbird has no
// threshold map. They are pinned instead against classForPercentage in
// TestClassForPercentage. The two default widgets must lead the slice: the
// implicit default group is Widgets in this order, so a caller relying on
// it must see 5h and week first.
func TestWeaverbirdSpec_Widgets(t *testing.T) {
	if weaverbirdSpec.Provider != "bloodhound" {
		t.Errorf("Provider = %q, want bloodhound", weaverbirdSpec.Provider)
	}
	if weaverbirdSpec.Icon != "🩸" {
		t.Errorf("Icon = %q, want the bloodhound icon", weaverbirdSpec.Icon)
	}
	if len(weaverbirdSpec.Widgets) != 4 {
		t.Fatalf("len(Widgets) = %d, want 4", len(weaverbirdSpec.Widgets))
	}
	if weaverbirdSpec.Widgets[0].ID != "bloodhound.5h" || weaverbirdSpec.Widgets[1].ID != "bloodhound.week" {
		t.Errorf("Widgets[0:2] = [%q, %q], want [bloodhound.5h, bloodhound.week] first (implicit default group order)",
			weaverbirdSpec.Widgets[0].ID, weaverbirdSpec.Widgets[1].ID)
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

// TestWeaverbirdSpec_Groups pins the "bloodhound.detail" group: all four
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
	want := []string{"bloodhound.5h", "bloodhound.week", "bloodhound.burn", "bloodhound.poll"}
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
			v := bucketValue("bloodhound.5h", "5h", tc.w)
			if v.ID != "bloodhound.5h" {
				t.Errorf("ID = %q, want bloodhound.5h", v.ID)
			}
			if v.Class != tc.wantClass {
				t.Errorf("Class = %q, want %q", v.Class, tc.wantClass)
			}
		})
	}
}

// TestBucketValue_TextMirrorsStatuslineStyle pins the text shape the ask
// calls for: label, percentage, and a parenthesized reset countdown, the
// same "72%/5h (3h12m)"-style pairing internal/statusline already prints,
// just spelled with the label first.
func TestBucketValue_TextMirrorsStatuslineStyle(t *testing.T) {
	w := &routes.NowWindow{Pct: 72, TimeToResetMS: int64(33 * time.Minute / time.Millisecond)}
	v := bucketValue("bloodhound.5h", "5h", w)
	if v.FullText != "5h 72% (33m)" {
		t.Errorf("Text = %q, want %q", v.FullText, "5h 72% (33m)")
	}
	if v.ShortText != "72%" {
		t.Errorf("Short = %q, want %q", v.ShortText, "72%")
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
