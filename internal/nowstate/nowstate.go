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
// PollIntervalS and StaleAfterS are the one pair of config-forwarded hints
// this package DOES set: a consumer comparing `bloodhound now --json`
// against GET /api/now needs both surfaces to agree on them, which is
// exactly the gap this file was extended to close, so Compute loads
// config and fills them in itself rather than leaving it to each caller.
//
// Everything else config-forwarded (active-session threshold,
// recent-session window), the in-window chart history, and the
// recent-sessions panel stay genuinely out of scope. Those are additions
// the Now page layers on top of this computation for its own purposes;
// they aren't part of what a consumer means by "pool state," and the HTTP
// handler still adds them itself after calling Compute.
package nowstate

import (
	"context"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
	"github.com/PeterSR/claude-code-bloodhound/internal/config"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

// Compute builds the pool-state fields of the /api/now payload: OK,
// Session, Week, LastPoll, NowMS, PollIntervalS, and StaleAfterS.
// Session/Week are nil when the latest observation didn't carry that
// bucket's percentage (never captured yet, or a misparse); LastPoll is nil
// only when there is no observation at all.
//
// The last two fields come from config.Load(), read here rather than
// passed in so the HTTP handler and the CLI can't each roll their own copy
// (see the package comment). A config problem (a corrupt file; a missing
// one already returns Default() with no error) must not fail pool state
// along with it, so on error both are just left at zero rather than
// aborting the computation.
// The two limit windows Claude Code meters. Named because buildWindow and
// fillBurn both need the span and must not be able to disagree about it.
const (
	sessionSpan = 5 * time.Hour
	weekSpan    = 7 * 24 * time.Hour
)

func Compute(ctx context.Context, s *store.Store, now time.Time) (*routes.NowResponse, error) {
	out := &routes.NowResponse{NowMS: now.UnixMilli()}

	if cfg, err := config.Load(); err == nil {
		out.PollIntervalS = cfg.PollIntervalS
		out.StaleAfterS = cfg.StaleAfterS
	}

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

	// When the newest poll produced no reading, fall back to the newest one
	// that did and mark the windows stale. LastPoll and OK above keep
	// reporting the real latest attempt, so "the last poll failed" is still
	// visible; what changes is that the failure no longer takes the
	// percentages down with it.
	//
	// A failed poll is usually transient (a capture that timed out while
	// the panel was still loading), and on a 5-minute interval the old
	// behaviour left every consumer with nil windows until the next
	// success. Consumers read nil as "no data ever", so a single dropped
	// poll made bloodhound look uninstalled rather than briefly behind.
	//
	// Only the whole observation is swapped, never mixed per bucket: the
	// two percentages and their resets are one reading of one panel, and
	// pairing a live week with a carried-over session would be a state
	// that never existed on screen.
	stale := false
	if !obs.ParseOK {
		prev, err := s.LatestParsedUsage(ctx)
		if err != nil {
			return nil, err
		}
		if prev == nil {
			return out, nil
		}
		obs = prev
		stale = true
	}

	if obs.SessionPct != nil {
		ws := buildWindow(*obs.SessionPct, obs.SessionResetTSISO, sessionSpan, obs.SessionResetDetected, now)
		ws.Saturated = obs.SessionSaturated
		fillBurn(ctx, s, ws, *obs.SessionPct, true, sessionSpan, now)
		markStale(ws, stale, obs.TSISO)
		out.Session = ws
	}

	if obs.WeekPct != nil {
		ws := buildWindow(*obs.WeekPct, obs.WeekResetTSISO, weekSpan, obs.WeekResetDetected, now)
		ws.Saturated = obs.WeekSaturated
		fillBurn(ctx, s, ws, *obs.WeekPct, false, weekSpan, now)
		markStale(ws, stale, obs.TSISO)
		out.Week = ws
	}

	return out, nil
}

// markStale tags a window as carried forward from tsISO. A no-op on the
// normal path, so the two bucket blocks above stay symmetric rather than
// each growing its own conditional.
func markStale(ws *routes.NowWindow, stale bool, tsISO string) {
	if !stale {
		return
	}
	ws.Stale = true
	ws.StaleTSISO = tsISO
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
