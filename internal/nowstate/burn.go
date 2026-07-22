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
func fillBurn(ctx context.Context, s pctSource, ws *routes.NowWindow, pct int, isSession bool, now time.Time) {
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
	// Suppress when the projected limit is after the natural reset — not
	// actionable, and tends to be noisy when the burn-rate window is
	// short.
	if ws.TimeToResetMS > 0 && etaMS >= ws.TimeToResetMS {
		return
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
	const oneHourMS = int64(3600 * 1000)
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
