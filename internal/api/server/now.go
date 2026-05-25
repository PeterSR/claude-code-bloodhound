package server

import (
	"context"
	"net/http"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
	"github.com/PeterSR/claude-code-bloodhound/internal/config"
	"github.com/PeterSR/claude-code-bloodhound/internal/sessioninsight"
)

func (s *Server) handleNow(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()
	out := routes.NowResponse{NowMS: now.UnixMilli()}
	recentWindowS := 86400
	if cfg, err := config.Load(); err == nil {
		out.PollIntervalS = cfg.PollIntervalS
		out.StaleAfterS = cfg.StaleAfterS
		out.ActiveSessionThresholdS = cfg.ActiveSessionThresholdS
		out.RecentSessionWindowS = cfg.RecentSessionWindowS
		if cfg.RecentSessionWindowS > 0 {
			recentWindowS = cfg.RecentSessionWindowS
		}
	}

	obs, err := s.Store.LatestUsage(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if obs == nil {
		writeJSON(w, http.StatusOK, out)
		return
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
		fillBurn(ctx, s.Store, ws, *obs.SessionPct, true, now)
		out.Session = ws
		if ws.WindowStartTSISO != "" {
			if start, err := time.Parse(time.RFC3339, ws.WindowStartTSISO); err == nil {
				out.SessionHistory = s.queryNowHistory(ctx, true, start.UnixMilli(), now.UnixMilli())
			}
		}
	}

	if obs.WeekPct != nil {
		ws := buildWindow(*obs.WeekPct, obs.WeekResetTSISO, 7*24*time.Hour, obs.WeekResetDetected, now)
		ws.Saturated = obs.WeekSaturated
		fillBurn(ctx, s.Store, ws, *obs.WeekPct, false, now)
		out.Week = ws
		if ws.WindowStartTSISO != "" {
			if start, err := time.Parse(time.RFC3339, ws.WindowStartTSISO); err == nil {
				out.WeekHistory = s.queryNowHistory(ctx, false, start.UnixMilli(), now.UnixMilli())
			}
		}
	}

	// Per-turn-derived insights for up to 10 most-recent sessions.
	// tokens_per_pct_cw drives the % estimates; pulled once and reused
	// across cards for consistency.
	tokensPerPct, _, _, hasCal, _ := s.Store.LatestCalibrationMedian(ctx, "session", 10)
	out.RecentSessions = s.recentSessionInsights(ctx, now, recentWindowS, tokensPerPct, hasCal)

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
func (s *Server) queryNowHistory(ctx context.Context, isSession bool, startMS, endMS int64) []routes.NowHistoryPoint {
	pctCol, satCol := "session_pct", "session_saturated"
	if !isSession {
		pctCol, satCol = "week_pct", "week_saturated"
	}
	rows, err := s.Store.DB.QueryContext(ctx, `
		SELECT ts_unix_ms, `+pctCol+`, `+satCol+`
		FROM usage_observations
		WHERE ts_unix_ms BETWEEN ? AND ?
		  AND parse_ok = 1
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

// fillBurn populates BurnOK / BurnPctPerHour and (only when actionable)
// LimitOK / LimitETAMS / LimitETATS.
func fillBurn(ctx context.Context, s storeIface, ws *routes.NowWindow, pct int, isSession bool, now time.Time) {
	var pts []burnPoint
	var err error
	if isSession {
		pts, err = readPoints(ctx, s, true)
	} else {
		pts, err = readPoints(ctx, s, false)
	}
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

func round2(f float64) float64 {
	return float64(int(f*100+0.5)) / 100
}
