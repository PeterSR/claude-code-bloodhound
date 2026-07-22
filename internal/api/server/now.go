package server

import (
	"context"
	"net/http"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
	"github.com/PeterSR/claude-code-bloodhound/internal/config"
	"github.com/PeterSR/claude-code-bloodhound/internal/nowstate"
	"github.com/PeterSR/claude-code-bloodhound/internal/sessioninsight"
)

// handleNow serves pool state (percentage, window reset, burn rate,
// saturation, last-poll age, and the poll-interval/stale-after config
// hints) plus a few UI-only additions the Now page needs on top of it. The
// pool state itself, hints included, comes from nowstate.Compute, the same
// function `bloodhound now` calls straight against SQLite: this handler
// and that CLI command can't drift on the numbers a consumer might be
// comparing across the two. Everything after the Compute call here
// (active-session threshold, recent-session window, chart history,
// recent-sessions panel) is UI-specific and deliberately outside what
// nowstate computes; see that package's doc comment for why.
func (s *Server) handleNow(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()

	out, err := nowstate.Compute(ctx, s.Store, now)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	recentWindowS := 86400
	if cfg, err := config.Load(); err == nil {
		out.ActiveSessionThresholdS = cfg.ActiveSessionThresholdS
		out.RecentSessionWindowS = cfg.RecentSessionWindowS
		if cfg.RecentSessionWindowS > 0 {
			recentWindowS = cfg.RecentSessionWindowS
		}
	}

	// History and recent-sessions only mean anything once there's at least
	// one observation to anchor a window to — out.LastPoll != nil is that
	// same condition nowstate.Compute already checked (mirrors the
	// pre-extraction "obs == nil" early return this handler used to make
	// itself).
	if out.LastPoll != nil {
		if out.Session != nil && out.Session.WindowStartTSISO != "" {
			if start, err := time.Parse(time.RFC3339, out.Session.WindowStartTSISO); err == nil {
				out.SessionHistory = s.queryNowHistory(ctx, true, start.UnixMilli(), now.UnixMilli())
			}
		}
		if out.Week != nil && out.Week.WindowStartTSISO != "" {
			if start, err := time.Parse(time.RFC3339, out.Week.WindowStartTSISO); err == nil {
				out.WeekHistory = s.queryNowHistory(ctx, false, start.UnixMilli(), now.UnixMilli())
			}
		}

		// Per-turn-derived insights for up to 10 most-recent sessions.
		// tokens_per_pct_cw drives the % estimates; pulled once and reused
		// across cards for consistency.
		tokensPerPct, _, _, hasCal, _ := s.Store.LatestCalibrationMedian(ctx, "session", 10)
		out.RecentSessions = s.recentSessionInsights(ctx, now, recentWindowS, tokensPerPct, hasCal)
	}

	writeJSON(w, http.StatusOK, out)
}

// recentSessionInsights wraps sessioninsight.RecentN + ForSession +
// LastUserPrompt for the Now page. A hard cap of recentSessionsMax keeps
// the panel from blowing up on days where the user has been jumping
// between many projects.
func (s *Server) recentSessionInsights(ctx context.Context, now time.Time, windowS int, tokensPerPct float64, hasCal bool) []sessioninsight.Insight {
	const recentSessionsMax = 10
	refs, err := sessioninsight.RecentN(ctx, s.Store.DB, now, windowS, recentSessionsMax)
	if err != nil || len(refs) == 0 {
		return nil
	}
	out := make([]sessioninsight.Insight, 0, len(refs))
	for _, ref := range refs {
		ins := sessioninsight.ForSession(ctx, s.Store.DB, ref, now, tokensPerPct, hasCal)
		if ins == nil {
			continue
		}
		ins.LastUserPrompt = sessioninsight.LastUserPrompt(ctx, s.Store.DB, ref.UUID)
		out = append(out, *ins)
	}
	return out
}

// queryNowHistory returns the pct + saturated series for one bucket within
// [startMS, endMS]. Errors collapse to an empty result — the chart is
// non-essential and we'd rather render the gauges than fail the page.
//
// Filtering only parse_ok let a flagged misparse (a >100 reading, or a dip
// that recovers next poll) through to the Now page's chart and its
// least-squares projection, so pct_valid = 1 is required here too.
//
// Saturated readings are a different case: they are real data, not junk,
// so they stay in this result set with their flag carried through — the
// chart shades those periods from routes.NowHistoryPoint.Saturated. What
// they must not do is drag the fitted slope toward zero right when the
// user is actually burning fastest, but that guard belongs to the fit
// itself (Now.tsx), not to what the endpoint returns. Same reasoning
// burnSeries and pctSince apply to their own inputs, just split across the
// wire instead of filtered out entirely.
func (s *Server) queryNowHistory(ctx context.Context, isSession bool, startMS, endMS int64) []routes.NowHistoryPoint {
	pctCol, satCol, validCol := "session_pct", "session_saturated", "session_pct_valid"
	if !isSession {
		pctCol, satCol, validCol = "week_pct", "week_saturated", "week_pct_valid"
	}
	rows, err := s.Store.DB.QueryContext(ctx, `
		SELECT ts_unix_ms, `+pctCol+`, `+satCol+`
		FROM usage_observations
		WHERE ts_unix_ms BETWEEN ? AND ?
		  AND parse_ok = 1
		  AND `+validCol+` = 1
		ORDER BY ts_unix_ms ASC
	`, startMS, endMS)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []routes.NowHistoryPoint
	for rows.Next() {
		var (
			ts     int64
			pctRaw any
			sat    int
		)
		if err := rows.Scan(&ts, &pctRaw, &sat); err != nil {
			return out
		}
		p := routes.NowHistoryPoint{TSUnixMS: ts, Saturated: sat == 1}
		if v, ok := nullableInt(pctRaw); ok {
			p.Pct = &v
		}
		out = append(out, p)
	}
	return out
}

// round2 rounds to 2 decimal places. Used package-wide (attribution.go,
// history.go, models.go, ...), not just here — buildWindow and fillBurn
// used to live in this file too and called it, but both moved to
// internal/nowstate (which keeps its own copy; see that package's
// comment).
func round2(f float64) float64 {
	return float64(int(f*100+0.5)) / 100
}
