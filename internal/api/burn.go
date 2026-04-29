package api

import "github.com/PeterSR/claude-code-bloodhound/internal/store"

// burnRate computes the burn rate (% per hour) over the most recent
// contiguous segment of post-reset observations within the last hour. Two
// or more points are required.
//
// Returns (slope, ok). When ok=false we don't have enough data.
func burnRate(points []store.PctPoint) (slopePctPerHour float64, ok bool) {
	if len(points) < 2 {
		return 0, false
	}
	// Window: last 60 minutes of the segment, or all of it if shorter.
	last := points[len(points)-1]
	const oneHourMS = 3600 * 1000
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

// etaTo100 returns the milliseconds-from-now until pct reaches 100 at the
// given slope. Negative or near-zero slopes return ok=false.
func etaTo100(currentPct int, slopePctPerHour float64) (etaMS int64, ok bool) {
	if slopePctPerHour <= 0.05 {
		return 0, false
	}
	remaining := 100.0 - float64(currentPct)
	if remaining <= 0 {
		return 0, true
	}
	hours := remaining / slopePctPerHour
	return int64(hours * 3600 * 1000), true
}
