package nowstate

import (
	"context"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

// pctSource is the minimal store surface fillBurn needs — a seam so tests
// can hand in fixed points instead of standing up a real database. Mirrors
// what internal/api/server/burn.go's now-removed storeIface provided
// before this computation moved here.
type pctSource interface {
	SessionPctSinceLastReset(ctx context.Context) ([]store.PctPoint, error)
	WeekPctSinceLastReset(ctx context.Context) ([]store.PctPoint, error)
}

// accountPctSource binds the store to one account's meter, so the seam
// above stays account-free and test fakes need not know accounts exist.
type accountPctSource struct {
	s  *store.Store
	id int64
}

func (a accountPctSource) SessionPctSinceLastReset(ctx context.Context) ([]store.PctPoint, error) {
	return a.s.SessionPctSinceLastReset(ctx, a.id)
}

func (a accountPctSource) WeekPctSinceLastReset(ctx context.Context) ([]store.PctPoint, error) {
	return a.s.WeekPctSinceLastReset(ctx, a.id)
}

func readPoints(ctx context.Context, s pctSource, session bool) ([]store.PctPoint, error) {
	if session {
		return s.SessionPctSinceLastReset(ctx)
	}
	return s.WeekPctSinceLastReset(ctx)
}

// fillBurn populates BurnOK / BurnPctPerHour and (only when actionable)
// LimitOK / LimitETAMS / LimitETATS.
//
// When there is no usable slope (fewer than two points, or slopeOver finds
// nothing to measure), BurnOK stays false and BurnPctPerHour stays unset.
// That omission is deliberate, not an oversight: a downstream consumer
// renders the absent case as "n/a", and reporting a literal 0 here would
// read as "measured calm" instead of "couldn't measure." Preserve this on
// every path through this function.
func fillBurn(ctx context.Context, s pctSource, ws *routes.NowWindow, pct int, isSession bool, span time.Duration, now time.Time) {
	pts, err := readPoints(ctx, s, isSession)
	if err != nil || len(pts) < 2 {
		return
	}

	slope, ok := slopeOver(pts)
	if !ok {
		return
	}
	ws.BurnPctPerHour = round2(slope)
	ws.BurnOK = true

	if slope <= 0.05 {
		return
	}
	remaining := 100 - pct
	if remaining <= 0 {
		return
	}
	etaMS := int64(float64(remaining) / slope * 3600 * 1000)
	// The three suppressions below all need a window shape. Without a parsed
	// reset there is none, and the projection goes out as measured, the same
	// thing it did before any of these gates existed.
	if ws.TimeToResetMS > 0 {
		// Suppress when the projected limit is after the natural reset — not
		// actionable, and tends to be noisy when the burn-rate window is
		// short.
		if etaMS >= ws.TimeToResetMS {
			return
		}
		elapsedMS := span.Milliseconds() - ws.TimeToResetMS
		if !offPace(pct, elapsedMS, span.Milliseconds()) {
			return
		}
		if !withinEvidence(etaMS, elapsedMS) {
			return
		}
	}
	ws.LimitOK = true
	ws.LimitETAMS = etaMS
	ws.LimitETATS = now.Add(time.Duration(etaMS) * time.Millisecond).UTC().Format(time.RFC3339)
}

// slopeOver returns (slope, ok) over the most recent contiguous segment of
// up-to-1-hour points. Two or more points required.
//
// Moved verbatim from internal/api/server/burn.go, which kept the same
// deliberate corrections this now-handler-turned-CLI-shared computation
// depends on: never differentiating across a reset (points is already
// scoped to "since the last reset" by SessionPctSinceLastReset /
// WeekPctSinceLastReset), never treating a saturated or flagged-misparse
// reading as an endpoint (same store methods already exclude those), and
// refusing to report a slope when the two endpoints are less than an
// instant apart (dtH <= 0).
func slopeOver(points []store.PctPoint) (slopePctPerHour float64, ok bool) {
	if len(points) < 2 {
		return 0, false
	}
	last := points[len(points)-1]
	cut := last.TSUnixMS - oneHourMS
	var window []store.PctPoint
	for _, p := range points {
		if p.TSUnixMS >= cut {
			window = append(window, p)
		}
	}
	if len(window) < 2 {
		window = points[len(points)-2:]
	}
	a, b := window[0], window[len(window)-1]
	dtH := float64(b.TSUnixMS-a.TSUnixMS) / 3600 / 1000
	if dtH <= 0 {
		return 0, false
	}
	return float64(b.Pct-a.Pct) / dtH, true
}

// oneHourMS is both the slope measurement window and the floor on how much
// evidence withinEvidence will credit a young window with.
const oneHourMS = int64(3600 * 1000)

// paceMargin is how far past a straight-line-to-100% pace a window has to be
// running before offPace calls it. Sitting exactly on pace means arriving at
// the cap exactly as the window resets, which is not a warning; and because
// pct is an integer that only climbs while the elapsed share climbs
// continuously, a bare "faster than pace" test crosses back and forth in
// place near the boundary and turns each crossing into a level event.
const paceMargin = 1.1

// evidenceMultiple is how far past the evidence span a projection may reach.
// Three hours of extrapolation off one hour of watching is a guess worth
// making; six days off the same hour is not.
const evidenceMultiple = 3

// offPace reports whether the window is being spent faster than it
// replenishes: extend the average since the window opened over the whole
// window and see whether it lands past the cap.
//
// This is what keeps the weekly bucket honest. A 168-hour window is spent by
// someone who sleeps, and the last hour of a working day extrapolated across
// six days always projects a crossing. The weaverbird weekly widget rendered
// danger at 2% used for exactly that reason. The average since the reset has
// the idle hours in it and the last hour does not.
//
// Deliberately permissive when there is no window shape to compare against:
// this gate exists to suppress a projection that the shape contradicts, not
// to require a shape before projecting at all.
func offPace(pct int, elapsedMS, spanMS int64) bool {
	if elapsedMS <= 0 || spanMS <= 0 {
		return true
	}
	projectedEndPct := float64(pct) * float64(spanMS) / float64(elapsedMS)
	return projectedEndPct >= 100*paceMargin
}

// withinEvidence reports whether an ETA is close enough to be supported by
// how long the window has been observed.
//
// offPace alone is not enough, because early in a window the elapsed share is
// so small that any use at all reads as off pace: an hour into the week, 2%
// used against a 0.6% elapsed share projects an end-of-week 336%. The measured
// average is real there, it just has nothing behind it yet. This gate is what
// makes the weekly projection quiet for the first day and responsive after it,
// without either bucket needing to know its own length.
func withinEvidence(etaMS, elapsedMS int64) bool {
	evidence := elapsedMS
	if evidence < oneHourMS {
		evidence = oneHourMS
	}
	return etaMS <= evidenceMultiple*evidence
}
