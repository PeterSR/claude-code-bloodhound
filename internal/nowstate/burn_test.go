package nowstate

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

const (
	minute = int64(60 * 1000)
	hour   = 60 * minute
)

// epoch is a fixed "now" for fillBurn tests where the exact wall-clock
// value doesn't matter, only its relation to the point timestamps.
var epoch = time.Unix(0, 0).UTC()

func pt(tsMS int64, pct int) store.PctPoint {
	return store.PctPoint{TSUnixMS: tsMS, Pct: pct}
}

func TestSlopeOver_FewerThanTwoPointsIsNotOK(t *testing.T) {
	if _, ok := slopeOver(nil); ok {
		t.Error("nil points: want ok=false")
	}
	if _, ok := slopeOver([]store.PctPoint{pt(0, 5)}); ok {
		t.Error("one point: want ok=false")
	}
}

func TestSlopeOver_BasicSlope(t *testing.T) {
	// 2% -> 10% over 30 minutes = 16%/hour.
	pts := []store.PctPoint{pt(0, 2), pt(30*minute, 10)}
	slope, ok := slopeOver(pts)
	if !ok {
		t.Fatal("want ok=true")
	}
	if math.Abs(slope-16) > 1e-9 {
		t.Errorf("slope = %v, want 16", slope)
	}
}

func TestSlopeOver_UsesOnlyTheLastHourWindow(t *testing.T) {
	// A point 90 minutes back sits outside the last-hour window: the slope
	// must be measured against the closest point still inside it (30 min
	// back), not the oldest one.
	pts := []store.PctPoint{
		pt(0, 0),          // 90 min before the last point — excluded
		pt(60*minute, 40), // 30 min before the last point — the window floor
		pt(90*minute, 46), // last point
	}
	slope, ok := slopeOver(pts)
	if !ok {
		t.Fatal("want ok=true")
	}
	// (46-40)/0.5h = 12%/hour, not (46-0)/1.5h = 30.67%/hour.
	if math.Abs(slope-12) > 1e-9 {
		t.Errorf("slope = %v, want 12 (measured from the in-window floor, not the oldest point)", slope)
	}
}

func TestSlopeOver_FallsBackToLastTwoPointsWhenNothingIsInsideTheHour(t *testing.T) {
	// Readings 2 hours apart: the last-hour window is empty, so slopeOver
	// falls back to the last two points rather than reporting nothing.
	pts := []store.PctPoint{pt(0, 0), pt(2*60*minute, 10)}
	slope, ok := slopeOver(pts)
	if !ok {
		t.Fatal("want ok=true (fallback to the last two points)")
	}
	if math.Abs(slope-5) > 1e-9 {
		t.Errorf("slope = %v, want 5 (10 points / 2 hours)", slope)
	}
}

func TestSlopeOver_ZeroSpanIsNotOK(t *testing.T) {
	// Two readings at the identical timestamp: dtH <= 0, refuse rather than
	// divide by (near) zero.
	pts := []store.PctPoint{pt(1000, 5), pt(1000, 9)}
	if _, ok := slopeOver(pts); ok {
		t.Error("zero-duration span: want ok=false")
	}
}

// fakePctSource lets fillBurn tests hand in fixed points instead of
// standing up a real store.
type fakePctSource struct {
	points []store.PctPoint
	err    error
}

func (f fakePctSource) SessionPctSinceLastReset(context.Context) ([]store.PctPoint, error) {
	return f.points, f.err
}

func (f fakePctSource) WeekPctSinceLastReset(context.Context) ([]store.PctPoint, error) {
	return f.points, f.err
}

// TestFillBurn_OmitsRatherThanZeroingWhenNoSlope is the behaviour the ask
// calls out as easy to regress: when there's no usable slope, BurnOK must
// stay false and BurnPctPerHour must stay unset (its Go zero value), never
// a computed 0. A downstream consumer renders the absent case as "n/a";
// reporting 0 would read as "measured calm" instead of "couldn't measure."
func TestFillBurn_OmitsRatherThanZeroingWhenNoSlope(t *testing.T) {
	cases := []struct {
		name string
		src  fakePctSource
	}{
		{"no points", fakePctSource{}},
		{"one point", fakePctSource{points: []store.PctPoint{pt(0, 12)}}},
		{"store error", fakePctSource{points: []store.PctPoint{pt(0, 1), pt(minute, 2)}, err: errBoom}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ws := &routes.NowWindow{Pct: 42}
			fillBurn(context.Background(), c.src, ws, 42, true, sessionSpan, epoch)
			if ws.BurnOK {
				t.Fatalf("BurnOK = true, want false (%s)", c.name)
			}
			if ws.BurnPctPerHour != 0 {
				t.Fatalf("BurnPctPerHour = %v, want the zero value 0 (unset), not a measured 0", ws.BurnPctPerHour)
			}

			// The same omission must survive JSON encoding: the field key
			// itself must be absent (omitempty), not present as 0.
			b, err := json.Marshal(ws)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if strings.Contains(string(b), "burn_pct_per_hour") {
				t.Errorf("JSON %s contains burn_pct_per_hour, want it omitted entirely", b)
			}
			if !strings.Contains(string(b), `"burn_ok":false`) {
				t.Errorf("JSON %s missing burn_ok:false", b)
			}
		})
	}
}

func TestFillBurn_PopulatesWhenASlopeExists(t *testing.T) {
	src := fakePctSource{points: []store.PctPoint{pt(0, 10), pt(30*minute, 18)}}
	ws := &routes.NowWindow{Pct: 18}
	fillBurn(context.Background(), src, ws, 18, true, sessionSpan, epoch)

	if !ws.BurnOK {
		t.Fatal("BurnOK = false, want true")
	}
	if math.Abs(ws.BurnPctPerHour-16) > 1e-9 {
		t.Errorf("BurnPctPerHour = %v, want 16 (8 points / 0.5h)", ws.BurnPctPerHour)
	}
}

func TestFillBurn_FlatSlopeSetsBurnOKButNoLimitProjection(t *testing.T) {
	// A slope at or under 0.05%/hour is close enough to flat that fillBurn
	// still reports it (BurnOK true, matching a real "not burning" reading)
	// but does not go on to project a limit ETA from it.
	src := fakePctSource{points: []store.PctPoint{pt(0, 10), pt(30*minute, 10)}}
	ws := &routes.NowWindow{Pct: 10}
	fillBurn(context.Background(), src, ws, 10, true, sessionSpan, epoch)

	if !ws.BurnOK {
		t.Fatal("BurnOK = false, want true (a measured, if flat, slope)")
	}
	if ws.BurnPctPerHour != 0 {
		t.Errorf("BurnPctPerHour = %v, want 0 (genuinely flat)", ws.BurnPctPerHour)
	}
	if ws.LimitOK {
		t.Error("LimitOK = true, want false (flat slope never projects a limit ETA)")
	}
}

// TestFillBurn_WeeklyDoesNotProjectFromOneBusyHour pins the bug the
// weaverbird weekly widget shipped with: one hour into the week, 2% used,
// and the widget rendered danger. A 1-hour slope stretched across a
// 168-hour window always finds a crossing, because nobody works 168 hours
// and the last hour does not know that.
//
// The measured rate itself is still reported. It is real, and "2%/hour right
// now" is a fine thing to show; what it cannot support is a claim about next
// Thursday.
func TestFillBurn_WeeklyDoesNotProjectFromOneBusyHour(t *testing.T) {
	src := fakePctSource{points: []store.PctPoint{pt(0, 0), pt(hour, 2)}}
	ws := &routes.NowWindow{Pct: 2, TimeToResetMS: 167 * hour} // one hour in
	fillBurn(context.Background(), src, ws, 2, false, weekSpan, epoch)

	if !ws.BurnOK {
		t.Fatal("BurnOK = false, want true (the hourly rate is measured and honest)")
	}
	if math.Abs(ws.BurnPctPerHour-2) > 1e-9 {
		t.Errorf("BurnPctPerHour = %v, want 2", ws.BurnPctPerHour)
	}
	if ws.LimitOK {
		t.Errorf("LimitOK = true (ETA %v), want false: an hour of evidence cannot reach 49 hours out", ws.LimitETAMS)
	}
}

// TestFillBurn_WeeklyOnPaceDoesNotProject is the same window four and a half
// days in, spending steadily and landing under the cap. The evidence gate is
// wide open by then, so this is the pace gate on its own: 54% used against a
// 66% elapsed share projects to 82% at reset, and 82% is not a warning.
func TestFillBurn_WeeklyOnPaceDoesNotProject(t *testing.T) {
	src := fakePctSource{points: []store.PctPoint{pt(0, 53), pt(hour, 54)}}
	ws := &routes.NowWindow{Pct: 54, TimeToResetMS: 57 * hour} // 111 hours in
	fillBurn(context.Background(), src, ws, 54, false, weekSpan, epoch)

	if !ws.BurnOK {
		t.Fatal("BurnOK = false, want true")
	}
	if ws.LimitOK {
		t.Errorf("LimitOK = true (ETA %v), want false: on pace to finish the week under the cap", ws.LimitETAMS)
	}
}

// TestFillBurn_WeeklyOffPaceStillProjects is the other half of the fix. The
// week must still be able to raise a hand. Quieting it permanently would
// have been the easy wrong answer. Two days in at 60% used projects to 210%
// at reset, which is worth saying.
func TestFillBurn_WeeklyOffPaceStillProjects(t *testing.T) {
	src := fakePctSource{points: []store.PctPoint{pt(0, 57), pt(hour, 60)}}
	ws := &routes.NowWindow{Pct: 60, TimeToResetMS: 120 * hour} // 48 hours in
	fillBurn(context.Background(), src, ws, 60, false, weekSpan, epoch)

	if !ws.LimitOK {
		t.Fatal("LimitOK = false, want true (off pace, with two days of evidence behind it)")
	}
	// 40 points remaining at 3%/hour.
	if want := int64(40.0 / 3 * float64(hour)); math.Abs(float64(ws.LimitETAMS-want)) > float64(minute) {
		t.Errorf("LimitETAMS = %v, want about %v", ws.LimitETAMS, want)
	}
}

// TestFillBurn_SessionWindowStillProjects guards the bucket that was never
// broken. A 1-hour slope reaching a few hours ahead inside a 5-hour window is
// exactly the case the projection was built for, and neither new gate should
// touch it.
func TestFillBurn_SessionWindowStillProjects(t *testing.T) {
	src := fakePctSource{points: []store.PctPoint{pt(0, 40), pt(hour, 60)}}
	ws := &routes.NowWindow{Pct: 60, TimeToResetMS: 3 * hour} // two hours in
	fillBurn(context.Background(), src, ws, 60, true, sessionSpan, epoch)

	if !ws.LimitOK {
		t.Fatal("LimitOK = false, want true (40 points left at 20%/hour, two hours before reset)")
	}
	if ws.LimitETATS == "" {
		t.Error("LimitETATS is empty, want a timestamp alongside the ETA")
	}
}

// TestFillBurn_NoWindowShapeProjectsAsMeasured covers the reading with no
// parsed reset. There is no reset to compare an ETA against and no elapsed
// share to weigh a pace against, so both new gates stand aside and the
// projection goes out as it did before they existed.
func TestFillBurn_NoWindowShapeProjectsAsMeasured(t *testing.T) {
	src := fakePctSource{points: []store.PctPoint{pt(0, 0), pt(hour, 2)}}
	ws := &routes.NowWindow{Pct: 2} // TimeToResetMS unset: reset never parsed
	fillBurn(context.Background(), src, ws, 2, false, weekSpan, epoch)

	if !ws.LimitOK {
		t.Fatal("LimitOK = false, want true (nothing known that could contradict the measurement)")
	}
}

func TestOffPace(t *testing.T) {
	cases := []struct {
		name      string
		pct       int
		elapsedMS int64
		want      bool
	}{
		{"half the week gone, half the week spent", 50, 84 * hour, false},
		{"exactly on pace is not a warning", 25, 42 * hour, false},
		{"a little ahead stays under the margin", 27, 42 * hour, false},
		{"well ahead", 40, 42 * hour, true},
		{"nothing elapsed yet", 5, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := offPace(c.pct, c.elapsedMS, weekSpan.Milliseconds()); got != c.want {
				t.Errorf("offPace(%d, %v) = %v, want %v", c.pct, time.Duration(c.elapsedMS)*time.Millisecond, got, c.want)
			}
		})
	}
}

func TestWithinEvidence(t *testing.T) {
	cases := []struct {
		name      string
		etaMS     int64
		elapsedMS int64
		want      bool
	}{
		{"three hours out on one hour of watching", 3 * hour, hour, true},
		{"forty-nine hours out on one hour of watching", 49 * hour, hour, false},
		{"a young window still gets its hour of credit", 3 * hour, 10 * minute, true},
		{"two days of watching reaches a long way", 100 * hour, 48 * hour, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := withinEvidence(c.etaMS, c.elapsedMS); got != c.want {
				t.Errorf("withinEvidence(%v, %v) = %v, want %v", c.etaMS, c.elapsedMS, got, c.want)
			}
		})
	}
}

var errBoom = &testErr{"boom"}

type testErr struct{ s string }

func (e *testErr) Error() string { return e.s }
