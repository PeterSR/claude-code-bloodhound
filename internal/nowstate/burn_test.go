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

const minute = int64(60 * 1000)

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
			fillBurn(context.Background(), c.src, ws, 42, true, epoch)
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
	fillBurn(context.Background(), src, ws, 18, true, epoch)

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
	fillBurn(context.Background(), src, ws, 10, true, epoch)

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

var errBoom = &testErr{"boom"}

type testErr struct{ s string }

func (e *testErr) Error() string { return e.s }
