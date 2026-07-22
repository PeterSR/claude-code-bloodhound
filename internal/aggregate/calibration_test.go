package aggregate

import (
	"math"
	"testing"
)

const (
	base   = int64(1750000000000) // fixed epoch; nothing here depends on wall clock
	minute = int64(60 * 1000)
	hour   = 60 * minute
)

// good builds an ordinary usable reading: has a pct, passes the misparse
// classifier, not saturated, not a reset. Tests override just the fields
// they care about.
func good(offset int64, pct int) calObs {
	return calObs{TSUnixMS: base + offset, Pct: pct, HasPct: true, Valid: true}
}

func turn(offset int64, cw float64) calTurn {
	return calTurn{TSUnixMS: base + offset, Raw: int64(cw), CW: cw}
}

func near(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > 0.001 {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// The headline fix: a flat interval (no observed movement) is not a reason
// to drop its tokens. A pairwise walk would skip the flat 10->10 pair
// entirely (delta <= 0) and lose its tokens on the floor; a run-based walk
// covers the whole flat-then-tick span in one point, so those tokens are
// still counted in the numerator.
func TestFlatThenTickIncludesTheFlatIntervalsTokens(t *testing.T) {
	obs := []calObs{
		good(0, 10),
		good(5*minute, 10),  // flat: a pairwise walk would have skipped this pair
		good(10*minute, 11), // ticks
	}
	turns := []calTurn{
		turn(1*minute, 100), // spent during the "flat" interval
		turn(6*minute, 100), // spent during the interval that ticked
	}

	pts := buildCalibrationPoints(obs, turns)

	if len(pts) != 1 {
		t.Fatalf("points = %d, want 1 (one run: no reset/saturation break)", len(pts))
	}
	p := pts[0]
	if p.DeltaPct != 1 {
		t.Fatalf("delta_pct = %d, want 1 (10 -> 11)", p.DeltaPct)
	}
	// Both turns (200 total) fall inside (first.ts, last.ts], not just the
	// 100 spent in the single interval that happened to tick.
	near(t, p.CostWeightedTokens, 200, "cost-weighted tokens must include the flat interval's spend")
	near(t, p.TokensPerPctCW, 200, "tokens per pct: 200cw / 1pt, not 100cw / 1pt")
}

// The other half of the fix: an oscillating meter (28, 30, 28, 30) must not
// count the second rise as fresh spend. It is only regaining ground the
// first rise already reached, so a walk with no peak memory would double it.
func TestOscillatingReadingsRecoveryContributesZero(t *testing.T) {
	obs := []calObs{
		good(0, 28),
		good(1*hour, 30),
		good(2*hour, 28), // dips back down
		good(3*hour, 30), // recovers to a peak already seen: not new spend
	}
	turns := []calTurn{
		turn(30*minute, 500),
		turn(90*minute, 500),
		turn(150*minute, 500),
	}

	pts := buildCalibrationPoints(obs, turns)

	if len(pts) != 1 {
		t.Fatalf("points = %d, want 1", len(pts))
	}
	p := pts[0]
	// True movement is 28 -> 30, once: 2 points. A pairwise walk with no
	// peak memory would see 28->30 (+2) then 28->30 again (+2) = 4.
	if p.DeltaPct != 2 {
		t.Errorf("delta_pct = %d, want 2 (peak only ever reaches 30 once)", p.DeltaPct)
	}
	if p.APct != 28 || p.BPct != 30 {
		t.Errorf("entry/exit peak = %d/%d, want 28/30", p.APct, p.BPct)
	}
}

// An invalid reading in the middle of a run (the dip a misparse produces)
// must not anchor a spurious drop, but it also must not sever a span that is
// otherwise perfectly measurable into two separately-priced halves.
func TestInvalidReadingNeitherAnchorsNorBreaksTheRun(t *testing.T) {
	bad := good(1*hour, 3) // e.g. a misread "34" as "3"
	bad.Valid = false
	obs := []calObs{
		good(0, 10),
		bad,
		good(2*hour, 25),
	}
	turns := []calTurn{turn(90*minute, 300)}

	pts := buildCalibrationPoints(obs, turns)

	if len(pts) != 1 {
		t.Fatalf("points = %d, want 1 (the invalid reading must not split the run)", len(pts))
	}
	p := pts[0]
	if p.APct != 10 || p.BPct != 25 {
		t.Errorf("entry/exit peak = %d/%d, want 10/25 (the invalid 3 must not anchor or move either)", p.APct, p.BPct)
	}
	if p.DeltaPct != 15 {
		t.Errorf("delta_pct = %d, want 15 (10 -> 25, skipping the misparse)", p.DeltaPct)
	}
	// The whole span's tokens land in this one point, not split around the
	// invalid reading, and the run's literal bounds include it even though
	// it didn't get to anchor anything.
	near(t, p.CostWeightedTokens, 300, "cost-weighted tokens across the invalid reading")
	if p.ATSUnixMS != base || p.BTSUnixMS != base+2*hour {
		t.Errorf("span = [%d, %d], want [%d, %d]", p.ATSUnixMS, p.BTSUnixMS, base, base+2*hour)
	}
}

// A detected reset starts a fresh window: the peak from before it must not
// carry forward and suppress the next window's real movement.
func TestResetForgetsThePeak(t *testing.T) {
	reset := good(1*hour, 2) // meter rolled back to near zero
	reset.Reset = true
	obs := []calObs{
		good(0, 90),
		reset,
		good(2*hour, 20),
	}
	turns := []calTurn{
		turn(90*minute, 400), // spent after the reset, before the next reading
	}

	pts := buildCalibrationPoints(obs, turns)

	if len(pts) != 1 {
		t.Fatalf("points = %d, want 1 (nothing before the reset had more than one reading to measure, so only the post-reset run prices)", len(pts))
	}
	p := pts[0]
	// The reset reading itself opens the new run at pct=2. Without the
	// peak-forgetting fix this would compare against the pre-reset peak of
	// 90 and see negative (clamped to zero) movement instead of 2 -> 20.
	if p.APct != 2 || p.BPct != 20 {
		t.Errorf("entry/exit peak = %d/%d, want 2/20 (peak must not carry across the reset)", p.APct, p.BPct)
	}
	if p.DeltaPct != 18 {
		t.Errorf("delta_pct = %d, want 18", p.DeltaPct)
	}
}

// A saturated reading breaks the run being measured, so nothing spent while
// the meter is pinned gets priced, but it must NOT reset the peak the way a
// detected reset does: once the meter is readable again, a dip back toward
// (but not above) the last known peak is still pure recovery, not fresh
// spend, exactly as it would be without the saturation gap in between.
func TestSaturatedReadingBreaksTheRunButKeepsThePeak(t *testing.T) {
	sat := good(1*hour, 99)
	sat.Saturated = true
	obs := []calObs{
		good(0, 10),
		good(30*minute, 90), // pre-cap run: real movement 10 -> 90
		sat,
		good(2*hour, 85), // post-cap: dips below the carried peak of 90
		good(3*hour, 95), // new ground beyond 90, but only 5 points of it
	}
	turns := []calTurn{
		turn(15*minute, 50),   // prices the pre-cap run
		turn(90*minute, 1000), // spent while pinned: must not be priced anywhere
		turn(150*minute, 300), // prices the post-cap run
	}

	pts := buildCalibrationPoints(obs, turns)

	if len(pts) != 2 {
		t.Fatalf("points = %d, want 2 (pre-cap run, post-cap run; nothing spans the saturated reading)", len(pts))
	}

	pre, post := pts[0], pts[1]
	if pre.APct != 10 || pre.BPct != 90 || pre.DeltaPct != 80 {
		t.Errorf("pre-cap run = %d->%d (delta %d), want 10->90 (delta 80)", pre.APct, pre.BPct, pre.DeltaPct)
	}
	near(t, pre.CostWeightedTokens, 50, "pre-cap tokens")

	// The dip to 85 must not lower the entry: it's still below the peak of
	// 90 carried in from before the cap, so the true new ground is only
	// 90 -> 95, not 85 -> 95.
	if post.APct != 90 || post.BPct != 95 || post.DeltaPct != 5 {
		t.Errorf("post-cap run = %d->%d (delta %d), want 90->95 (delta 5): the carried peak must survive the saturation break", post.APct, post.BPct, post.DeltaPct)
	}
	// The 1000cw spent while pinned must land in neither point.
	near(t, post.CostWeightedTokens, 300, "post-cap tokens must exclude the saturated interval's spend")
	total := pre.CostWeightedTokens + post.CostWeightedTokens
	near(t, total, 350, "combined tokens must never include the 1000cw spent while capped")
}

// A run with no usable reading at all (every observation in it invalid) must
// not clobber the carried peak. The run after it should still measure
// against the last real high point, bridging the gap instead of losing it.
func TestRunWithNoUsableReadingLeavesThePeakUntouched(t *testing.T) {
	sat1 := good(1*hour, 0)
	sat1.Saturated = true
	badA := good(90*minute, 5)
	badA.Valid = false
	badB := good(100*minute, 4)
	badB.Valid = false
	sat2 := good(110*minute, 0)
	sat2.Saturated = true

	obs := []calObs{
		good(0, 10),
		good(30*minute, 50), // establishes a peak of 50
		sat1,
		badA, badB, // an entire run with nothing to anchor it
		sat2,
		good(2*hour, 45),           // below the carried peak of 50: pure recovery
		good(2*hour+10*minute, 48), // still below 50: still pure recovery
	}
	turns := []calTurn{turn(15*minute, 200)}

	pts := buildCalibrationPoints(obs, turns)

	if len(pts) != 1 {
		t.Fatalf("points = %d, want 1: the all-invalid run emits nothing, and the peak of 50 it must have preserved "+
			"makes the trailing 45/48 run pure recovery (also nothing)", len(pts))
	}
	p := pts[0]
	if p.APct != 10 || p.BPct != 50 || p.DeltaPct != 40 {
		t.Errorf("first run = %d->%d (delta %d), want 10->50 (delta 40)", p.APct, p.BPct, p.DeltaPct)
	}
	near(t, p.CostWeightedTokens, 200, "first run's tokens")
}
