package server

import (
	"math"
	"testing"
)

const minute = int64(60 * 1000)

// mkPoints builds a usable, unbroken run of readings `every` apart.
func mkPoints(start int64, every int64, pcts ...int) []ratePoint {
	out := make([]ratePoint, len(pcts))
	for i, p := range pcts {
		out[i] = ratePoint{TSUnixMS: start + int64(i)*every, Pct: p, Usable: true}
	}
	return out
}

func TestBurnSeriesMeasuresSteadyRate(t *testing.T) {
	// 2% every 15 minutes = 8%/hour, held for the whole run.
	pts := mkPoints(0, 15*minute, 0, 2, 4, 6, 8, 10)
	got := burnSeries(pts, 45*minute)

	if got[0] != nil {
		t.Errorf("first point should have no baseline, got %v", *got[0])
	}
	for i := 1; i < len(got); i++ {
		if got[i] == nil {
			t.Errorf("point %d: nil, want 8%%/hour", i)
			continue
		}
		if math.Abs(*got[i]-8) > 1e-9 {
			t.Errorf("point %d: %g%%/hour, want 8", i, *got[i])
		}
	}
}

func TestBurnSeriesNeverDifferentiatesAcrossAReset(t *testing.T) {
	// The failure this guards: a window climbs to 95%, resets to 1%, and a
	// naive derivative reports a -94-point cliff that dwarfs every real
	// feature on the chart.
	pts := mkPoints(0, 15*minute, 80, 90, 95)
	pts = append(pts, ratePoint{
		TSUnixMS: 45 * minute, Pct: 1, Usable: true, SegmentBreak: true,
	})
	pts = append(pts, ratePoint{TSUnixMS: 60 * minute, Pct: 3, Usable: true})

	got := burnSeries(pts, 45*minute)

	if got[3] != nil {
		t.Errorf("reset point: %g%%/hour, want nil (no baseline in the new window)", *got[3])
	}
	if got[4] == nil {
		t.Fatal("point after reset: nil, want a slope measured within the new window")
	}
	// 1% -> 3% over 15 min, measured against the reset point only.
	if math.Abs(*got[4]-8) > 1e-9 {
		t.Errorf("point after reset: %g%%/hour, want 8", *got[4])
	}
}

func TestBurnSeriesSkipsSaturated(t *testing.T) {
	// While saturated the percentage is pinned at the cap, so its
	// derivative is a flat 0 that would read as "not burning."
	pts := mkPoints(0, 15*minute, 50, 60, 70)
	pts = append(pts,
		ratePoint{TSUnixMS: 45 * minute, Pct: 100, Usable: false},
		ratePoint{TSUnixMS: 60 * minute, Pct: 100, Usable: false},
	)
	got := burnSeries(pts, 45*minute)

	for _, i := range []int{3, 4} {
		if got[i] != nil {
			t.Errorf("saturated point %d: %g%%/hour, want nil", i, *got[i])
		}
	}
	if got[2] == nil || math.Abs(*got[2]-40) > 1e-9 {
		t.Errorf("pre-saturation slope wrong: %v", got[2])
	}
}

func TestBurnSeriesRejectsBaselinesTooShortToMeanAnything(t *testing.T) {
	// Readings one minute apart: a single 1% rounding tick would read as
	// 60%/hour. Refuse rather than chart noise.
	pts := mkPoints(0, minute, 10, 11, 12)
	got := burnSeries(pts, 45*minute)
	for i, v := range got {
		if v != nil {
			t.Errorf("point %d: %g%%/hour, want nil (baseline under %d min)",
				i, *v, burnMinSpanMS/minute)
		}
	}
}

func TestBurnSeriesRejectsBaselinesTooLongToMeanAnything(t *testing.T) {
	// A 6-hour gap exceeds burnMaxSpanMS. Averaging across it would draw a
	// confident flat line over a period we know nothing about.
	pts := []ratePoint{
		{TSUnixMS: 0, Pct: 10, Usable: true},
		{TSUnixMS: 6 * 60 * minute, Pct: 40, Usable: true},
	}
	if got := burnSeries(pts, 45*minute); got[1] != nil {
		t.Errorf("6h baseline: %g%%/hour, want nil", *got[1])
	}
}

func TestBurnSeriesFallsBackWhenPollingIsSparserThanLookback(t *testing.T) {
	// Readings every 90 min with a 45-min lookback: nothing is ever inside
	// the window, but the data is still perfectly chartable.
	pts := mkPoints(0, 90*minute, 0, 6, 12)
	got := burnSeries(pts, 45*minute)
	for i := 1; i < len(got); i++ {
		if got[i] == nil {
			t.Fatalf("point %d: nil, want a fallback slope", i)
		}
		if math.Abs(*got[i]-4) > 1e-9 {
			t.Errorf("point %d: %g%%/hour, want 4", i, *got[i])
		}
	}
}

func TestBurnSeriesUsesLongestBaselineInsideLookback(t *testing.T) {
	// The smoothing claim: with a 45-min lookback, the last point must be
	// measured against t-45 rather than its immediate neighbor. Here one
	// reading is a rounding blip; the long baseline should ride over it.
	pts := mkPoints(0, 15*minute, 0, 5, 5, 12)
	got := burnSeries(pts, 45*minute)

	// Against t=0 (45 min back): 12% / 0.75h = 16%/hour.
	// Against t=30 (the neighbor): 7% / 0.25h = 28%/hour — the blip.
	if got[3] == nil {
		t.Fatal("last point: nil")
	}
	if math.Abs(*got[3]-16) > 1e-9 {
		t.Errorf("last point: %g%%/hour, want 16 (measured over the full lookback)", *got[3])
	}
}

func TestBurnSeriesLookbackFloor(t *testing.T) {
	// A caller asking for 1-minute smoothing gets burnMinSpanMS anyway;
	// otherwise the API could be asked to publish pure quantization noise.
	pts := mkPoints(0, 15*minute, 0, 2, 4)
	got := burnSeries(pts, minute)
	if got[2] == nil {
		t.Fatal("want a slope: the floor should widen the lookback, not disable it")
	}
	if math.Abs(*got[2]-8) > 1e-9 {
		t.Errorf("got %g%%/hour, want 8", *got[2])
	}
}

func TestBurnSeriesTreatsOnePointJitterAsZeroNotNegative(t *testing.T) {
	// Observed in real data: an idle session reads 18, 17, 17, 18. Integer
	// rounding permits a 1-point drop around a flat true value, so the
	// honest rate is zero. A limit window cannot burn at a negative rate.
	pts := mkPoints(0, 15*minute, 18, 17, 17, 18)
	got := burnSeries(pts, 45*minute)
	for i := 1; i < len(got); i++ {
		if got[i] == nil {
			t.Errorf("point %d: nil, want 0 (jitter is not missing data)", i)
			continue
		}
		if *got[i] < 0 {
			t.Errorf("point %d: %g%%/hour, want >= 0", i, *got[i])
		}
	}
}

func TestBurnSeriesSkipsFlaggedMisparseAndDoesNotUseItAsBaseline(t *testing.T) {
	// The store classifies a misparse (usage_flags.go) and hands the burn
	// chart the reading already marked unusable. The chart's whole job here
	// is to honor that: skip the bad reading's own slot AND never measure a
	// later reading against it. This is the case that actually showed on the
	// real chart — the bogus 16 became the baseline for a legitimate 34 and
	// manufactured a +24%/hour weekly climb 45 minutes later.
	//
	// A flat 34 with one flagged misparse in the middle. Nothing is burning.
	pts := mkPoints(0, 15*minute, 34, 34, 16, 34, 34, 34, 34)
	pts[2].Usable = false // the store flagged it session_pct_valid = 0

	got := burnSeries(pts, 45*minute)

	if got[2] != nil {
		t.Errorf("flagged misparse: %g%%/hour, want nil (it's unusable)", *got[2])
	}
	for i, v := range got {
		if v != nil && *v != 0 {
			t.Errorf("point %d: %g%%/hour, want 0 — a flat 34 is not burning", i, *v)
		}
	}
}

func TestBurnSeriesBreaksAtAFlaggedReset(t *testing.T) {
	// A reset arrives as SegmentBreak (the store detected the 18 -> 0 drop
	// that the old threshold missed). The chart must start a fresh baseline,
	// never differentiate the rollback into a downward cliff.
	pts := mkPoints(0, 15*minute, 8, 14, 18)
	pts = append(pts,
		ratePoint{TSUnixMS: 45 * minute, Pct: 0, Usable: true, SegmentBreak: true},
		ratePoint{TSUnixMS: 60 * minute, Pct: 2, Usable: true},
	)
	got := burnSeries(pts, 45*minute)

	if got[3] != nil {
		t.Errorf("reset point: %g%%/hour, want nil (no baseline in the new window)", *got[3])
	}
	if got[4] == nil || *got[4] < 0 {
		t.Errorf("after reset: %v, want a non-negative slope within the new window", got[4])
	}
}

func TestBurnSeriesEmpty(t *testing.T) {
	if got := burnSeries(nil, 45*minute); len(got) != 0 {
		t.Errorf("nil input: got %d values, want 0", len(got))
	}
}
