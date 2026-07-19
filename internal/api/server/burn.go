package server

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

const msPerHour = float64(3600 * 1000)

const (
	// burnMinSpanMS is the shortest baseline the derivative accepts.
	// /usage reports whole percents, so across a short span a single point
	// of rounding dominates the answer: a 1% tick two minutes apart reads
	// as 30%/hour. Ten minutes puts the rounding error under 6%/hour.
	burnMinSpanMS = int64(10 * 60 * 1000)

	// burnMaxSpanMS caps the fallback baseline. Averaging a rate across
	// half a day hides everything that happened inside it, and the flat
	// line it draws looks like knowledge rather than the absence of it.
	burnMaxSpanMS = int64(4 * 60 * 60 * 1000)
)

// ratePoint is one (time, percentage) reading feeding the burn-rate
// derivative. Its Usable and SegmentBreak flags come straight from the
// store's reset/misparse classification (usage_flags.go) — the burn chart
// trusts that single source of truth rather than re-detecting resets and
// bad readings itself.
type ratePoint struct {
	TSUnixMS int64
	Pct      int
	// Usable is false when the reading must not participate: no value,
	// saturated (pinned at the cap, so its slope is a serene 0%/hour at the
	// moment the user is burning hardest), or a flagged misparse.
	Usable bool
	// SegmentBreak marks the first reading of a new limit window.
	// Differentiating across a reset renders the rollback to zero as a
	// cliff steep enough to flatten every real feature on the chart.
	SegmentBreak bool
}

// burnSeries converts percentage readings into a trailing rate of change
// in percentage points per hour, one value per input point.
//
// Each point is measured against the earliest reading still inside
// lookback and still inside the same limit window. The long baseline is
// the whole point: /usage is integer-quantized, so differentiating
// adjacent readings mostly measures rounding. Widening the baseline
// divides that fixed ±1% error by a larger span until the real signal
// outweighs it.
//
// Points with no honest slope available (first of a window, saturated, or
// too short a baseline) come back nil, so the chart breaks the line rather
// than inventing one.
func burnSeries(pts []ratePoint, lookbackMS int64) []*float64 {
	if lookbackMS < burnMinSpanMS {
		lookbackMS = burnMinSpanMS
	}
	out := make([]*float64, len(pts))

	// segStart is the first index of the current limit window; lo is the
	// earliest index still inside the lookback. Both only move forward,
	// which keeps this linear.
	segStart, lo := 0, 0
	for i, p := range pts {
		if p.SegmentBreak {
			segStart, lo = i, i
		}
		if !p.Usable {
			// A hole, not a boundary. An unusable reading tells us nothing
			// about this one, but it doesn't invalidate the readings before
			// it — the window is still the same window. Only SegmentBreak
			// ends a window.
			continue
		}
		for lo < i && (!pts[lo].Usable || p.TSUnixMS-pts[lo].TSUnixMS > lookbackMS) {
			lo++
		}
		base := lo
		if base >= i {
			// Readings are sparser than the smoothing window. Fall back to
			// the most recent usable one so slow polling still charts;
			// burnMaxSpanMS below rejects it if the gap is too wide to
			// mean anything.
			base = -1
			for k := i - 1; k >= segStart; k-- {
				if pts[k].Usable {
					base = k
					break
				}
			}
		}
		if base < 0 || base >= i {
			continue
		}
		span := p.TSUnixMS - pts[base].TSUnixMS
		if span < burnMinSpanMS || span > burnMaxSpanMS {
			continue
		}
		rate := float64(p.Pct-pts[base].Pct) / (float64(span) / msPerHour)
		if rate < 0 {
			// The only drop that survives upstream flagging is a 1-point
			// rounding jitter around a real rate of roughly zero. Zero is
			// the floor a limit window can physically burn at.
			rate = 0
		}
		out[i] = &rate
	}
	return out
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
