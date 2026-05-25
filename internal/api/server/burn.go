package api

import (
	"context"

	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

// burnPoint is a tiny local alias so the now-handler isn't tightly coupled
// to a specific store type in tests.
type burnPoint = store.PctPoint

// storeIface exposes the queries readPoints needs.
type storeIface interface {
	SessionPctSinceLastReset(ctx context.Context) ([]store.PctPoint, error)
	WeekPctSinceLastReset(ctx context.Context) ([]store.PctPoint, error)
}

func readPoints(ctx context.Context, s storeIface, session bool) ([]burnPoint, error) {
	if session {
		return s.SessionPctSinceLastReset(ctx)
	}
	return s.WeekPctSinceLastReset(ctx)
}

// slopeOver returns (slope, ok) over the most recent contiguous segment of
// up-to-1-hour points. Two or more points required.
func slopeOver(points []burnPoint) (slopePctPerHour float64, ok bool) {
	if len(points) < 2 {
		return 0, false
	}
	last := points[len(points)-1]
	const oneHourMS = int64(3600 * 1000)
	cut := last.TSUnixMS - oneHourMS
	var window []burnPoint
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
