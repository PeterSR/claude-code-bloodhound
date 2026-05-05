package api

import (
	"context"
	"net/http"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/config"
	"github.com/PeterSR/claude-code-bloodhound/internal/sessioninsight"
)

// NowResponse is everything the "Now" page needs in one payload.
type NowResponse struct {
	OK       bool         `json:"ok"`
	Session  *windowState `json:"session"`
	Week     *windowState `json:"week"`
	LastPoll *pollSummary `json:"last_poll"`
	NowMS    int64        `json:"server_now_ms"`

	// PollIntervalS is the configured cadence between /usage scrapes. The
	// UI uses it to decide whether the latest poll is "fresh" — e.g. to
	// suppress redundant "now" annotations on the chart.
	PollIntervalS int `json:"poll_interval_s,omitempty"`
	StaleAfterS   int `json:"stale_after_s,omitempty"`

	// ActiveSessionThresholdS is the cutoff (seconds) below which a
	// session's age earns the Active badge on the Now page. Forwarded
	// from config so the UI doesn't need to call /api/settings.
	ActiveSessionThresholdS int `json:"active_session_threshold_s,omitempty"`

	// RecentSessionWindowS bounds which sessions are listed in the
	// recent-sessions panel. Forwarded so the UI can label the section
	// accurately ("Last 24h" etc.).
	RecentSessionWindowS int `json:"recent_session_window_s,omitempty"`

	// SessionHistory and WeekHistory are observation series within each
	// current window — anchored to [window_start_ts, reset_ts]. Empty
	// when the matching window is unknown (no parsed reset).
	SessionHistory []nowHistoryPoint `json:"session_history,omitempty"`
	WeekHistory    []nowHistoryPoint `json:"week_history,omitempty"`

	// RecentSessions is up to 5 most-recent sessions, each with full
	// per-turn insights (last-turn cost, recent-3 vs session-average,
	// compaction recommendation). Trailing entries that are far older
	// than the cluster are dropped so a stale list doesn't pad out the
	// panel.
	RecentSessions []sessioninsight.Insight `json:"recent_sessions,omitempty"`
}

// nowHistoryPoint is one observation slimmed for the in-window chart.
type nowHistoryPoint struct {
	TSUnixMS  int64 `json:"ts_unix_ms"`
	Pct       *int  `json:"pct,omitempty"`
	Saturated bool  `json:"saturated"`
}

// windowState describes one bucket. Two distinct time concepts to keep
// straight:
//
//   - Reset (always shown when known): the natural cycle boundary parsed
//     from /usage. "We are inside [window_start_ts, reset_ts]."
//   - Limit (only shown when burn-rate projection says we'd hit 100% before
//     reset): the actionable warning. Hidden otherwise — projecting "100%
//     in 8 days" when the bucket resets in 3 hours adds noise, not signal.
type windowState struct {
	Pct              int    `json:"pct"`
	ResetTSISO       string `json:"reset_ts,omitempty"`
	WindowStartTSISO string `json:"window_start_ts,omitempty"`
	TimeToResetMS    int64  `json:"time_to_reset_ms,omitempty"`

	BurnPctPerHour float64 `json:"burn_pct_per_hour,omitempty"`
	BurnOK         bool    `json:"burn_ok"`

	// Limit fields are non-zero only when LimitOK is true (i.e. positive
	// slope AND projected limit is before reset_ts). Frontend can
	// confidently render "⚠ 100% in X" iff LimitOK.
	LimitOK    bool   `json:"limit_ok"`
	LimitETAMS int64  `json:"limit_eta_ms,omitempty"`
	LimitETATS string `json:"limit_eta_ts,omitempty"`

	ResetDetected bool `json:"reset_detected_in_last_obs"`

	// Saturated marks that this bucket is at or above the saturation
	// threshold (≥99%). When true the user is past the included quota
	// and on Anthropic's pay-per-use "Extra usage" tier; pct stops
	// moving even though tokens keep being spent. UI surfaces this so
	// the gauge doesn't silently lie.
	Saturated bool `json:"saturated"`
}

type pollSummary struct {
	TSISO    string  `json:"ts"`
	AgeS     int64   `json:"age_s"`
	ParseOK  bool    `json:"parse_ok"`
	ElapsedS float64 `json:"elapsed_s"`
}

func (s *Server) handleNow(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()
	out := NowResponse{NowMS: now.UnixMilli()}
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
	out.LastPoll = &pollSummary{
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
func (s *Server) queryNowHistory(ctx context.Context, isSession bool, startMS, endMS int64) []nowHistoryPoint {
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
	var out []nowHistoryPoint
	for rows.Next() {
		var (
			ts     int64
			pctRaw any
			sat    int
		)
		if err := rows.Scan(&ts, &pctRaw, &sat); err != nil {
			return out
		}
		p := nowHistoryPoint{TSUnixMS: ts, Saturated: sat == 1}
		if v, ok := nullableInt(pctRaw); ok {
			p.Pct = &v
		}
		out = append(out, p)
	}
	return out
}

func buildWindow(pct int, resetISO string, span time.Duration, resetDetected bool, now time.Time) *windowState {
	ws := &windowState{Pct: pct, ResetDetected: resetDetected}
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
func fillBurn(ctx context.Context, s storeIface, ws *windowState, pct int, isSession bool, now time.Time) {
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
