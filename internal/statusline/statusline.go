// Package statusline renders the one-line summary that `bloodhound status`
// prints. Designed to be invoked by Claude Code's statusline command — it
// reruns on every conversation-state change (each prompt + each assistant
// response), so this code MUST be fast (DB reads + formatting, no I/O
// beyond that).
package statusline

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/config"
	"github.com/PeterSR/claude-code-bloodhound/internal/sessioninsight"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

// Render returns the single-line status string. Never returns an error;
// when the DB has nothing useful, returns a "no data" form so the caller
// can still print something.
//
// Format: "🩸 72%/5h (3h12m) · 68%/wk (1d18h)" where parens are the time
// until the bucket's natural reset (parsed from /usage). When the burn-rate
// projection says we'd hit 100% before that reset, a "⚠100% in 26m" warning
// is inserted: "🩸 72%/5h ⚠100% in 26m (3h12m) · …".
//
// When the most-recent session is still active, one compact per-session
// alert is appended (cold cache, cache about to expire, /compact
// recommended, or "running heavy"). sessionUUID is an optional hint —
// Claude Code pipes its session_id into the statusline command, and
// passing it lets us target the current session exactly instead of just
// "most recent". Empty string falls back to most-recent-active.
func Render(ctx context.Context, cfg config.Config, s *store.Store, now time.Time, sessionUUID string) string {
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
	age := time.Duration(now.UnixMilli()-obs.TSUnixMS) * time.Millisecond
	if age >= staleAfter {
		return prefix + "STALE " + fmtDur(age)
	}

	if !obs.ParseOK {
		return prefix + "extraction failed"
	}

	parts := []string{}

	if obs.SessionPct != nil {
		sessPts, _ := s.SessionPctSinceLastReset(ctx)
		parts = append(parts, formatBucket(*obs.SessionPct, "5h",
			obs.SessionResetTSISO, sessPts, now))
	}
	if obs.WeekPct != nil {
		weekPts, _ := s.WeekPctSinceLastReset(ctx)
		parts = append(parts, formatBucket(*obs.WeekPct, "wk",
			obs.WeekResetTSISO, weekPts, now))
	}

	if seg := formatActiveSession(ctx, cfg, s, now, sessionUUID); seg != "" {
		parts = append(parts, seg)
	}

	if len(parts) == 0 {
		return prefix + "no data"
	}
	return prefix + strings.Join(parts, " · ")
}

// formatActiveSession returns a single compact alert about the
// most-recent active session, or "" when nothing is worth surfacing.
// If sessionUUID is non-empty and present in the local store, that
// session is preferred; otherwise we fall back to the most-recent
// session within cfg.ActiveSessionThresholdS.
func formatActiveSession(ctx context.Context, cfg config.Config, s *store.Store, now time.Time, sessionUUID string) string {
	threshold := cfg.ActiveSessionThresholdS
	if threshold <= 0 {
		threshold = 1800
	}

	var ref *sessioninsight.SessionRef
	if sessionUUID != "" {
		ref, _ = sessioninsight.BySessionUUID(ctx, s.DB, sessionUUID)
	}
	if ref == nil {
		ref, _ = sessioninsight.MostRecentActive(ctx, s.DB, now, threshold)
	}
	if ref == nil {
		return ""
	}

	// Even when sessionUUID matched, gate alerts on activity — a stale
	// session matched by id is still stale, and "cold cache" on a
	// session you walked away from hours ago is just noise.
	ageS := (now.UnixMilli() - ref.LastMS) / 1000
	if ageS > int64(threshold) {
		return ""
	}

	tokensPerPct, _, _, hasCal, _ := s.LatestCalibrationMedian(ctx, "session", 10)
	ins := sessioninsight.ForSession(ctx, s.DB, *ref, now, tokensPerPct, hasCal)
	return formatActiveSegment(ins)
}

// formatActiveSegment picks one alert from an Insight in priority order.
// Pure function for testing — no DB or context dependencies.
//
// Priority (highest first):
//
//   - cold cache  → "❄ cold +5%"     (or "❄ cold" without calibration)
//   - expiring    → "⏱ 47s"
//   - /compact    → "⚠ /compact +12%" (or "⚠ /compact" without calibration)
//   - watch       → "↗ 1.7×"
//
// Cache state outranks bloat state because cold-resume is an immediate
// cost the next turn pays — /compact can wait a turn or two.
func formatActiveSegment(ins *sessioninsight.Insight) string {
	if ins == nil {
		return ""
	}
	if ins.ColdResumeCostCWTokens > 0 {
		if pct := renderPct(ins.ColdResumeCostPct); pct != "" {
			return "❄ cold +" + pct + "%"
		}
		return "❄ cold"
	}
	if ins.CacheExpiresInS > 0 {
		return "⏱ " + fmtDur(time.Duration(ins.CacheExpiresInS)*time.Second)
	}
	if ins.Recommendation == "compact" {
		if pct := renderPct(ins.CompactCostPct); pct != "" {
			return "⚠ /compact +" + pct + "%"
		}
		return "⚠ /compact"
	}
	if ins.Recommendation == "watch" && ins.SessionAvgCWTokens > 0 {
		ratio := ins.Recent3AvgCWTokens / ins.SessionAvgCWTokens
		return fmt.Sprintf("↗ %.1f×", ratio)
	}
	return ""
}

// renderPct renders a small percentage as a short integer ("5"), or
// "<1" when sub-percent. Empty string when v ≤ 0 (no calibration).
func renderPct(v float64) string {
	if v <= 0 {
		return ""
	}
	if v < 1 {
		return "<1"
	}
	return strconv.Itoa(int(v + 0.5))
}

// formatBucket builds one bucket's segment. Anchors on the parsed reset_ts
// where available, optionally prefixed with a burn-rate warning when a
// projection would hit 100% before that reset.
func formatBucket(pct int, label, resetISO string, points []store.PctPoint, now time.Time) string {
	timeToReset, hasReset := timeUntil(resetISO, now)

	// Burn-rate projection (only meaningful as a warning when it would
	// fire before the natural reset).
	limitETA, hasLimit := etaToLimit(points, pct)
	if hasReset && hasLimit && limitETA >= timeToReset {
		hasLimit = false // limit is after reset; not actionable
	}

	out := fmt.Sprintf("%d%%/%s", pct, label)
	if hasLimit {
		out += " ⚠100% in " + fmtDur(limitETA)
	}
	if hasReset {
		out += " (" + fmtDur(timeToReset) + ")"
	}
	return out
}

// timeUntil parses an ISO-8601 timestamp and returns the duration from now
// to that time, or (0, false) on failure / past timestamps.
func timeUntil(iso string, now time.Time) (time.Duration, bool) {
	if iso == "" {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return 0, false
	}
	d := t.Sub(now)
	if d <= 0 {
		return 0, false
	}
	return d, true
}

// etaToLimit returns the time until 100% based on the slope over the last
// hour of pct points. Returns (0, false) when the slope is too flat or
// the inputs aren't sufficient.
func etaToLimit(points []store.PctPoint, currentPct int) (time.Duration, bool) {
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
