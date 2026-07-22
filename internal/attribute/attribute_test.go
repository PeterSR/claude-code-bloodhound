package attribute

import (
	"math"
	"testing"
	"time"
)

const (
	base   = int64(1750000000000) // fixed epoch; nothing here depends on wall clock
	minute = int64(60 * 1000)
	hour   = 60 * minute
	day    = 24 * hour
)

// obsAt builds a weekly-bucket reading whose advertised reset is resetIn
// ahead of it. Anything the test does not name is a good reading.
func obsAt(offset int64, pct int, resetIn time.Duration) Obs {
	return Obs{
		TSUnixMS: base + offset,
		Pct:      pct,
		HasPct:   true,
		Valid:    true,
		ResetTS:  time.UnixMilli(base + offset + int64(resetIn/time.Millisecond)).UTC().Format(time.RFC3339),
	}
}

func turnAt(offset int64, uuid, project string, cw float64) Turn {
	return Turn{TSUnixMS: base + offset, SessionUUID: uuid, Project: project, RawTokens: int64(cw), CWTokens: cw}
}

func weekParams(tokensPerPct float64) Params {
	return ParamsFor(BucketWeek, base+30*day, tokensPerPct)
}

// find returns the row for a session in a given window.
func find(t *testing.T, res Result, windowStart int64, uuid string) Row {
	t.Helper()
	for _, r := range res.Rows {
		if r.WindowStartUnixMS == windowStart && r.SessionUUID == uuid {
			return r
		}
	}
	t.Fatalf("no row for session %q in window %d; rows=%+v", uuid, windowStart, res.Rows)
	return Row{}
}

func near(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > 0.01 {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// The headline behaviour: measured meter movement is split between the
// sessions that were running, in proportion to cost-weighted tokens.
func TestMeasuredSplitIsProRataByCostWeight(t *testing.T) {
	obs := []Obs{
		obsAt(0, 10, 7*24*time.Hour),
		obsAt(hour, 20, 7*24*time.Hour),
	}
	turns := []Turn{
		turnAt(10*minute, "a", "alpha", 300),
		turnAt(20*minute, "b", "beta", 100),
	}

	res := Build(obs, turns, weekParams(0))

	if len(res.Windows) != 1 {
		t.Fatalf("windows = %d, want 1", len(res.Windows))
	}
	w := res.Windows[0]
	near(t, w.MeasuredPct, 10, "window measured")
	near(t, w.AttributedPct, 10, "window attributed")

	// 300 : 100 of the 10 points the meter actually moved.
	near(t, find(t, res, w.StartUnixMS, "a").MeasuredPct, 7.5, "a measured")
	near(t, find(t, res, w.StartUnixMS, "b").MeasuredPct, 2.5, "b measured")
	if got := find(t, res, w.StartUnixMS, "a").EstimatedPct; got != 0 {
		t.Errorf("a estimated = %v, want 0 (the span was measured)", got)
	}
	if !w.Partial {
		t.Error("the oldest window should be partial: the series began mid-flight")
	}
}

// A window opened by a real reset starts at zero, so its first run must
// baseline at zero and reach back to the window start; otherwise everything
// spent before the window's first poll is silently lost.
func TestWindowAfterResetBaselinesAtZeroAndReachesBack(t *testing.T) {
	obs := []Obs{
		obsAt(0, 50, time.Hour),                      // old window, resets an hour out
		obsAt(2*hour, 3, 7*24*time.Hour+2*time.Hour), // reset jumped: new window
		obsAt(3*hour, 12, 7*24*time.Hour+time.Hour),
	}
	// Spent 5 minutes into the new window, well before its first reading.
	turns := []Turn{turnAt(65*minute, "c", "gamma", 500)}

	res := Build(obs, turns, weekParams(0))

	if len(res.Windows) != 2 {
		t.Fatalf("windows = %d, want 2 (the advertised reset jumped a week)", len(res.Windows))
	}
	second := res.Windows[1]
	if second.Partial {
		t.Error("a window opened by an observed reset is not partial")
	}
	if second.StartUnixMS != base+hour {
		t.Errorf("window start = %d, want the old window's advertised reset (%d)",
			second.StartUnixMS-base, hour)
	}
	// Meter went 0 -> 12 and only session c was running.
	near(t, find(t, res, second.StartUnixMS, "c").MeasuredPct, 12, "c measured")
	near(t, second.MeasuredPct, 12, "window measured")
}

// Movement no ingested turn explains must survive as its own row rather than
// inflating whoever happened to be running.
func TestUnattributedRemainderIsKept(t *testing.T) {
	obs := []Obs{
		obsAt(0, 10, 7*24*time.Hour),
		obsAt(hour, 18, 7*24*time.Hour),
	}

	res := Build(obs, nil, weekParams(0))

	w := res.Windows[0]
	near(t, find(t, res, w.StartUnixMS, Unattributed).MeasuredPct, 8, "unattributed")
	if r := find(t, res, w.StartUnixMS, Unattributed); r.TurnCount != 0 || r.CWTokens != 0 {
		t.Errorf("unattributed row carries tokens (%d turns, %v cw); it should carry only percent",
			r.TurnCount, r.CWTokens)
	}
}

// Turns past the last reading are real spend the meter has not reported yet.
// They fall back to the calibration median, and stay labelled as estimated.
func TestTailAfterLastObservationIsEstimated(t *testing.T) {
	obs := []Obs{
		obsAt(0, 10, 7*24*time.Hour),
		obsAt(hour, 12, 7*24*time.Hour),
	}
	turns := []Turn{
		turnAt(30*minute, "a", "alpha", 400),
		turnAt(2*hour, "b", "beta", 1000), // after the series ends
	}

	res := Build(obs, turns, weekParams(500)) // 500 cost-weighted tokens per point

	w := res.Windows[0]
	near(t, find(t, res, w.StartUnixMS, "a").MeasuredPct, 2, "a measured")
	b := find(t, res, w.StartUnixMS, "b")
	near(t, b.EstimatedPct, 2, "b estimated") // 1000 / 500
	if b.MeasuredPct != 0 {
		t.Errorf("b measured = %v, want 0: no reading brackets it", b.MeasuredPct)
	}
}

// While the meter is pinned at the cap it stops reporting spend, so nothing
// may be measured across it; those turns fall to the estimate instead.
func TestSaturatedReadingBreaksTheMeasuredRun(t *testing.T) {
	sat := obsAt(hour, 99, 7*24*time.Hour)
	sat.Saturated = true
	obs := []Obs{
		obsAt(0, 90, 7*24*time.Hour),
		sat,
		obsAt(2*hour, 99, 7*24*time.Hour),
	}
	turns := []Turn{
		turnAt(30*minute, "a", "alpha", 100),
		turnAt(90*minute, "b", "beta", 1000), // spent while capped
	}

	res := Build(obs, turns, weekParams(500))

	w := res.Windows[0]
	if !w.HitCap {
		t.Error("window should be flagged as having hit the cap")
	}
	b := find(t, res, w.StartUnixMS, "b")
	near(t, b.EstimatedPct, 2, "b estimated")
	if b.MeasuredPct != 0 {
		t.Errorf("b measured = %v, want 0: it spent while the meter was pinned", b.MeasuredPct)
	}
}

// A misparsed reading is not allowed to anchor anything, but it is also not
// allowed to sever a span that is otherwise perfectly measurable.
func TestInvalidReadingDoesNotBreakTheRun(t *testing.T) {
	bad := obsAt(hour, 4, 7*24*time.Hour) // the dip a misparse produces
	bad.Valid = false
	obs := []Obs{
		obsAt(0, 10, 7*24*time.Hour),
		bad,
		obsAt(2*hour, 30, 7*24*time.Hour),
	}
	turns := []Turn{turnAt(90*minute, "a", "alpha", 100)}

	res := Build(obs, turns, weekParams(500))

	w := res.Windows[0]
	near(t, w.MeasuredPct, 20, "window measured across the misparse")
	near(t, find(t, res, w.StartUnixMS, "a").MeasuredPct, 20, "a measured")
}

// An install that has never captured /usage still gets a complete breakdown,
// entirely from the calibration median, on a synthesized window grid.
func TestNoObservationsFallsBackToSyntheticWindows(t *testing.T) {
	turns := []Turn{
		turnAt(0, "a", "alpha", 500),
		turnAt(8*day, "b", "beta", 1500),
	}

	res := Build(nil, turns, weekParams(500))

	if len(res.Windows) != 2 {
		t.Fatalf("windows = %d, want 2 (the turns are 8 days apart)", len(res.Windows))
	}
	for _, w := range res.Windows {
		if !w.Inferred {
			t.Errorf("window %d should be flagged inferred", w.StartUnixMS-base)
		}
	}
	near(t, find(t, res, res.Windows[0].StartUnixMS, "a").EstimatedPct, 1, "a estimated")
	near(t, find(t, res, res.Windows[1].StartUnixMS, "b").EstimatedPct, 3, "b estimated")
}

// Turns older than the observation series get windows stepped back from the
// oldest real one, so history predating collection is still broken out.
func TestTurnsBeforeTheSeriesGetGriddedWindows(t *testing.T) {
	obs := []Obs{
		obsAt(0, 10, 7*24*time.Hour),
		obsAt(hour, 12, 7*24*time.Hour),
	}
	turns := []Turn{turnAt(-3*day, "old", "alpha", 1000)}

	res := Build(obs, turns, weekParams(500))

	if len(res.Windows) != 2 {
		t.Fatalf("windows = %d, want 2", len(res.Windows))
	}
	prior := res.Windows[0]
	if !prior.Inferred {
		t.Error("the pre-collection window should be flagged inferred")
	}
	if prior.StartUnixMS != base-7*day {
		t.Errorf("prior window start = %dd, want -7d", (prior.StartUnixMS-base)/day)
	}
	near(t, find(t, res, prior.StartUnixMS, "old").EstimatedPct, 2, "old estimated")
}

// A session that spans a weekly reset is two rows, one per window, so its
// cost lands in the week that actually paid for it.
func TestSessionStraddlingAResetSplitsAcrossWindows(t *testing.T) {
	obs := []Obs{
		obsAt(0, 40, time.Hour),
		obsAt(2*hour, 5, 7*24*time.Hour+2*time.Hour),
		obsAt(3*hour, 9, 7*24*time.Hour+time.Hour),
	}
	turns := []Turn{
		turnAt(30*minute, "a", "alpha", 100),
		turnAt(150*minute, "a", "alpha", 100),
	}

	res := Build(obs, turns, weekParams(0))

	if len(res.Windows) != 2 {
		t.Fatalf("windows = %d, want 2", len(res.Windows))
	}
	first := find(t, res, res.Windows[0].StartUnixMS, "a")
	second := find(t, res, res.Windows[1].StartUnixMS, "a")
	if first.TurnCount != 1 || second.TurnCount != 1 {
		t.Errorf("turns split %d/%d, want 1/1", first.TurnCount, second.TurnCount)
	}
	near(t, second.MeasuredPct, 9, "second week's share")
}

// The 5h bucket uses the same machinery with a shorter period and tighter
// reset thresholds; a reset only four hours out must still open a window.
func TestFiveHourBucketSegmentsOnItsOwnThresholds(t *testing.T) {
	mk := func(offset int64, pct int, resetIn time.Duration) Obs {
		o := obsAt(offset, pct, resetIn)
		return o
	}
	obs := []Obs{
		mk(0, 60, 30*time.Minute),
		mk(hour, 8, 5*time.Hour),
		mk(2*hour, 25, 4*time.Hour),
	}
	turns := []Turn{turnAt(90*minute, "a", "alpha", 100)}

	res := Build(obs, turns, ParamsFor(Bucket5h, base+day, 0))

	if len(res.Windows) != 2 {
		t.Fatalf("windows = %d, want 2", len(res.Windows))
	}
	if got := res.Windows[1].Bucket; got != Bucket5h {
		t.Errorf("bucket = %q, want %q", got, Bucket5h)
	}
	near(t, find(t, res, res.Windows[1].StartUnixMS, "a").MeasuredPct, 25, "a's share of the 5h window")
}

// Unmeasurable spend is priced by what the meter actually charged nearby,
// not by the caller's calibration median. The median passed here is
// deliberately four times too cheap (the shape of the bias the
// calibration_points table has) and must lose to the reconciled rate.
func TestEstimateUsesReconciledRateNotTheSuppliedMedian(t *testing.T) {
	obs := []Obs{
		obsAt(0, 10, 7*24*time.Hour),
		obsAt(2*hour, 30, 7*24*time.Hour),
	}
	turns := []Turn{
		turnAt(hour, "a", "alpha", 20000), // measured: 20000 cw moved 20 points
		turnAt(3*hour, "b", "beta", 5000), // past the series: must price at 1000/pt
	}

	res := Build(obs, turns, weekParams(250))

	w := res.Windows[0]
	near(t, w.TokensPerPctCW, 1000, "reconciled rate")
	near(t, res.TokensPerPctCW, 1000, "result rate")
	near(t, find(t, res, w.StartUnixMS, "b").EstimatedPct, 5, "b estimated at the reconciled rate")
}

// With nothing measurable anywhere, the caller's median is all there is.
func TestSuppliedMedianIsTheLastResort(t *testing.T) {
	res := Build(nil, []Turn{turnAt(0, "a", "alpha", 1000)}, weekParams(250))

	near(t, find(t, res, res.Windows[0].StartUnixMS, "a").EstimatedPct, 4, "a estimated")
	near(t, res.TokensPerPctCW, 250, "result rate falls back")
}

// When every run in a window is proportionally consistent with the window's
// own reconciled rate, the cap must never bite: capTolerance is > 1, so a
// run whose own cost-per-point is at or above the window's blended rate
// always clears cap >= delta. Nothing should be diverted to unattributed.
// A saturated reading splits the window into two separately-measured runs
// (see measureWindow) so this actually exercises two spans, not one.
func TestLocalTurnsWithinCapAreFullyAttributed(t *testing.T) {
	sat := obsAt(90*minute, 99, 7*24*time.Hour)
	sat.Saturated = true
	obs := []Obs{
		obsAt(0, 10, 7*24*time.Hour),
		obsAt(hour, 18, 7*24*time.Hour), // run A ends: 8 points
		sat,
		obsAt(2*hour, 18, 7*24*time.Hour), // run B starts where run A's peak left off
		obsAt(3*hour, 22, 7*24*time.Hour), // run B ends: 4 more points
	}
	turns := []Turn{
		// Both runs cost exactly 1000 cw per point, same as the window's
		// blended rate, so neither one is capped.
		turnAt(30*minute, "a", "alpha", 8000), // run A: 8 points @ 1000/pt
		turnAt(150*minute, "b", "beta", 4000), // run B: 4 points @ 1000/pt
	}

	res := Build(obs, turns, weekParams(0))

	w := res.Windows[0]
	near(t, w.MeasuredPct, 12, "window measured")
	near(t, w.TokensPerPctCW, 1000, "reconciled rate")
	near(t, find(t, res, w.StartUnixMS, "a").MeasuredPct, 8, "a measured, uncapped")
	near(t, find(t, res, w.StartUnixMS, "b").MeasuredPct, 4, "b measured, uncapped")
	for _, r := range res.Rows {
		if r.SessionUUID == Unattributed && r.Pct() != 0 {
			t.Errorf("unattributed = %v, want 0: local turns fully explain the movement", r.Pct())
		}
	}
}

// A span whose local turns cost far less than the window's own rate implies
// (the shape of a second machine on the same account moving the meter inside
// a span that also happens to hold one small local turn) must have the
// excess above capTolerance*totalCW/rate filed as unattributed, not charged
// to the local turn. The window must still reconcile exactly. As above, a
// saturated reading separates the well-explained run from the barely
// explained one so they are priced (and capped) as distinct spans.
func TestExcessBeyondCapIsUnattributed(t *testing.T) {
	sat := obsAt(90*minute, 99, 7*24*time.Hour)
	sat.Saturated = true
	obs := []Obs{
		obsAt(0, 10, 7*24*time.Hour),
		obsAt(hour, 18, 7*24*time.Hour), // run A: 8 points, well explained
		sat,
		obsAt(2*hour, 18, 7*24*time.Hour),
		obsAt(3*hour, 20, 7*24*time.Hour), // run B: 2 more points, barely explained
	}
	turns := []Turn{
		turnAt(30*minute, "a", "alpha", 8000), // run A: 8000 cw for 8 points
		turnAt(150*minute, "b", "beta", 20),   // run B: 20 cw for 2 points
	}

	res := Build(obs, turns, weekParams(0))

	w := res.Windows[0]
	// Window rate reconciles over BOTH runs: (8000+20) cw / (8+2) pts = 802.
	const rate = 802.0
	near(t, w.TokensPerPctCW, rate, "reconciled rate")

	// Run A's own cost-per-point (1000) is above the blended rate, so it is
	// never capped: fully attributed.
	near(t, find(t, res, w.StartUnixMS, "a").MeasuredPct, 8, "a fully attributed")

	// Run B's 20 cw caps out at capTolerance*20/rate points; the rest of its
	// 2-point delta cannot be explained by that turn and must be unattributed.
	capB := capTolerance * 20 / rate
	bRow := find(t, res, w.StartUnixMS, "b")
	near(t, bRow.MeasuredPct, capB, "b capped at its own plausible share")
	if bRow.MeasuredPct >= 2 {
		t.Errorf("b measured = %v, want well under the full 2-point delta it shares a span with", bRow.MeasuredPct)
	}

	unattr := find(t, res, w.StartUnixMS, Unattributed)
	near(t, unattr.MeasuredPct, 2-capB, "excess filed as unattributed")

	// The whole point of the cap: the window still reconciles exactly.
	var sum float64
	for _, r := range res.Rows {
		sum += r.Pct()
	}
	near(t, sum, w.MeasuredPct, "rows + unattributed sum to window movement")
	near(t, w.MeasuredPct, 10, "window movement") // 8 + 2
}

// A window that never moves enough to price itself (below minRatioPct) has
// no reconciled rate, so no cap can be computed and behaviour must be
// unchanged: a lone tiny local turn still absorbs the whole delta, exactly
// as it did before this cap existed.
func TestNoCapWithoutReconciledRate(t *testing.T) {
	obs := []Obs{
		obsAt(0, 10, 7*24*time.Hour),
		obsAt(hour, 11, 7*24*time.Hour), // only 1 point: below minRatioPct
	}
	turns := []Turn{turnAt(30*minute, "a", "alpha", 1)} // 1 cw, nowhere near 1 point's worth

	res := Build(obs, turns, weekParams(0))

	w := res.Windows[0]
	if w.TokensPerPctCW != 0 {
		t.Fatalf("window rate = %v, want 0: movement is below minRatioPct", w.TokensPerPctCW)
	}
	near(t, find(t, res, w.StartUnixMS, "a").MeasuredPct, 1, "a absorbs the full delta uncapped")
	for _, r := range res.Rows {
		if r.SessionUUID == Unattributed {
			t.Errorf("unattributed row present (%v pct); with no reconciled rate no cap is computable", r.Pct())
		}
	}
}

// Every window's session shares must add back up to what the meter moved:
// that reconciliation is the whole point of measuring rather than estimating.
func TestSharesReconcileWithWindowMovement(t *testing.T) {
	obs := []Obs{
		obsAt(0, 5, 7*24*time.Hour),
		obsAt(hour, 11, 7*24*time.Hour),
		obsAt(2*hour, 26, 7*24*time.Hour),
		obsAt(3*hour, 31, 7*24*time.Hour),
	}
	turns := []Turn{
		turnAt(10*minute, "a", "alpha", 137),
		turnAt(70*minute, "b", "beta", 921),
		turnAt(80*minute, "a", "alpha", 44),
		turnAt(160*minute, "c", "gamma", 611),
	}

	res := Build(obs, turns, weekParams(0))

	w := res.Windows[0]
	var sum float64
	for _, r := range res.Rows {
		sum += r.Pct()
	}
	near(t, sum, w.MeasuredPct, "sum of shares vs window movement")
	near(t, w.MeasuredPct, 26, "window movement") // 31 - 5
}
