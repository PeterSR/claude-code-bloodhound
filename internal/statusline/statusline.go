// Package statusline renders the one-line summary that `bloodhound status`
// prints. Designed to be invoked by Claude Code's statusline command, which
// runs the binary on every keystroke — so this code MUST be fast (DB read +
// formatting, no I/O beyond that).
package statusline

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/config"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

// Render returns the single-line status string. Never returns an error;
// when the DB has nothing useful, returns a "no data" form so the caller
// can still print something.
func Render(ctx context.Context, cfg config.Config, s *store.Store, now time.Time) string {
	prefix := cfg.StatuslinePrefix
	if prefix != "" {
		prefix += " "
	}

	obs, err := s.LatestUsage(ctx)
	if err != nil || obs == nil {
		return prefix + "no data"
	}

	staleAfter := time.Duration(cfg.StaleAfterS) * time.Second
	if staleAfter <= 0 {
		staleAfter = 10 * time.Minute
	}
	ageS := now.UnixMilli() - obs.TSUnixMS
	age := time.Duration(ageS) * time.Millisecond
	if age >= staleAfter {
		return prefix + fmt.Sprintf("STALE %s", fmtDur(age))
	}

	if !obs.ParseOK {
		return prefix + "extraction failed"
	}

	parts := []string{}
	if obs.SessionPct != nil {
		bit := fmt.Sprintf("%d%%/5h", *obs.SessionPct)
		// ETA from session burn rate.
		if pts, err := s.SessionPctSinceLastReset(ctx); err == nil {
			if eta, ok := etaFromPoints(pts, *obs.SessionPct); ok {
				bit += " " + fmtDur(eta) + " left"
			}
		}
		parts = append(parts, bit)
	}
	if obs.WeekPct != nil {
		parts = append(parts, fmt.Sprintf("%d%%/wk", *obs.WeekPct))
	}

	if len(parts) == 0 {
		return prefix + "no data"
	}
	return prefix + strings.Join(parts, " · ")
}

// etaFromPoints computes the ms until 100% based on the slope over the
// last hour of pct points. Returns false if we don't have a meaningful
// positive slope.
func etaFromPoints(points []store.PctPoint, currentPct int) (time.Duration, bool) {
	if len(points) < 2 {
		return 0, false
	}
	const oneHourMS = int64(3600 * 1000)
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
	slope := float64(b.Pct-a.Pct) / dtH
	if slope <= 0.05 {
		return 0, false
	}
	remaining := 100 - currentPct
	if remaining <= 0 {
		return 0, true
	}
	return time.Duration(float64(remaining)/slope*float64(time.Hour)) * 1, true
}

// fmtDur renders short relative durations: "12s", "8m", "2h12m", "1d4h".
func fmtDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	}
	day := int(d.Hours()) / 24
	rh := int(d.Hours()) - day*24
	if rh == 0 {
		return fmt.Sprintf("%dd", day)
	}
	return fmt.Sprintf("%dd%dh", day, rh)
}
