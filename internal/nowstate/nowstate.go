// Package nowstate computes the pool-state half of /api/now: current
// percentage per limit bucket, its window reset, time to that reset, burn
// rate, saturation, and last-poll freshness. It reads straight from the
// store, no daemon required.
//
// This exists so the HTTP handler (internal/api/server/now.go) and the
// `bloodhound now` CLI command share exactly one implementation. Before
// the extraction, the handler computed all of this inline; a CLI built by
// copying that logic would have been a second place for the burn-rate
// corrections in particular (see burn.go in this package) to quietly drift
// out of sync with the endpoint a downstream consumer might be comparing
// it against.
//
// Deliberately out of scope: config-forwarded UI hints (poll interval,
// stale-after threshold, ...), the in-window chart history, and the
// recent-sessions panel. Those are additions the Now page layers on top of
// this computation for its own purposes; they aren't part of what a
// consumer means by "pool state," and the HTTP handler still adds them
// itself after calling Compute.
package nowstate

import (
	"context"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

// Compute builds the pool-state fields of the /api/now payload: OK,
// Session, Week, LastPoll, and NowMS. Session/Week are nil when the latest
// observation didn't carry that bucket's percentage (never captured yet,
// or a misparse); LastPoll is nil only when there is no observation at
// all.
func Compute(ctx context.Context, s *store.Store, now time.Time) (*routes.NowResponse, error) {
	out := &routes.NowResponse{NowMS: now.UnixMilli()}

	obs, err := s.LatestUsage(ctx)
	if err != nil {
		return nil, err
	}
	if obs == nil {
		return out, nil
	}
	out.OK = obs.ParseOK
	out.LastPoll = &routes.NowPoll{
		TSISO:    obs.TSISO,
		AgeS:     (out.NowMS - obs.TSUnixMS) / 1000,
		ParseOK:  obs.ParseOK,
		ElapsedS: obs.ElapsedS,
	}

	if obs.SessionPct != nil {
		ws := buildWindow(*obs.SessionPct, obs.SessionResetTSISO, 5*time.Hour, obs.SessionResetDetected, now)
		ws.Saturated = obs.SessionSaturated
		fillBurn(ctx, s, ws, *obs.SessionPct, true, now)
		out.Session = ws
	}

	if obs.WeekPct != nil {
		ws := buildWindow(*obs.WeekPct, obs.WeekResetTSISO, 7*24*time.Hour, obs.WeekResetDetected, now)
		ws.Saturated = obs.WeekSaturated
		fillBurn(ctx, s, ws, *obs.WeekPct, false, now)
		out.Week = ws
	}

	return out, nil
}

// buildWindow derives reset / window-start / time-to-reset from a parsed
// reset timestamp. Left mostly bare (just Pct and ResetDetected) when the
// reset couldn't be parsed — the caller still has a percentage to show,
// just no window shape to hang off it.
func buildWindow(pct int, resetISO string, span time.Duration, resetDetected bool, now time.Time) *routes.NowWindow {
	ws := &routes.NowWindow{Pct: pct, ResetDetected: resetDetected}
	if resetISO == "" {
		return ws
	}
	t, err := time.Parse(time.RFC3339, resetISO)
	if err != nil {
		return ws
	}
	ws.ResetTSISO = t.UTC().Format(time.RFC3339)
	ws.WindowStartTSISO = t.Add(-span).UTC().Format(time.RFC3339)
	if d := t.Sub(now); d > 0 {
		ws.TimeToResetMS = d.Milliseconds()
	}
	return ws
}

// round2 rounds to 2 decimal places. Deliberately re-declared here rather
// than exported from internal/api/server: it's a one-line formatting
// helper, not part of the computation this package exists to single-source
// (same tradeoff cmd/bloodhound/attribution.go already makes for its own
// copy — see that file's comment).
func round2(f float64) float64 {
	return float64(int(f*100+0.5)) / 100
}
