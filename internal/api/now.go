package api

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/config"
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
	RecentSessions []sessionInsight `json:"recent_sessions,omitempty"`
}

// sessionInsight is a per-session summary with everything the Now page
// needs to render an insight card: identity + headline counts + per-turn
// cost trend + a /compact recommendation. All token figures are computed
// by SQL aggregation over the `turns` table.
type sessionInsight struct {
	SessionUUID  string `json:"session_uuid"`
	Project      string `json:"project"`
	LastTSISO    string `json:"last_ts"`
	LastTSUnixMS int64  `json:"last_ts_unix_ms"`
	AgeS         int64  `json:"age_s"`

	TurnCount      int     `json:"turn_count"`
	TotalRawTokens int64   `json:"total_raw_tokens"`
	TotalCWTokens  float64 `json:"total_cw_tokens"`

	// Last turn's actual cost.
	LastTurnRawTokens int64   `json:"last_turn_raw_tokens"`
	LastTurnCWTokens  float64 `json:"last_turn_cw_tokens"`
	LastTurnPct       float64 `json:"last_turn_pct,omitempty"` // estimated using TokensPerPctCW

	// Recent3AvgCWTokens averages the last min(3, turn_count) turns. The
	// UI compares this to SessionAvgCWTokens to flag context bloat.
	Recent3AvgCWTokens float64 `json:"recent3_avg_cw_tokens,omitempty"`
	Recent3AvgPct      float64 `json:"recent3_avg_pct,omitempty"`
	SessionAvgCWTokens float64 `json:"session_avg_cw_tokens,omitempty"`
	SessionAvgPct      float64 `json:"session_avg_pct,omitempty"`

	// TurnsSinceCompact = turns since the most recent post_compact=1 turn,
	// or TurnCount if this session has never been compacted.
	TurnsSinceCompact int `json:"turns_since_compact,omitempty"`

	// CompactCostCWTokens / CompactCostPct estimate what running /compact
	// right now would cost. /compact mechanically resembles one normal
	// turn (current context as input, summary as output), so we use the
	// smoothed recent-turn cost as a first-order estimate. Only set when
	// the recommendation suggests compacting could be worthwhile — the
	// field is the answer to "how much does it cost to act on this?"
	CompactCostCWTokens float64 `json:"compact_cost_cw_tokens,omitempty"`
	CompactCostPct      float64 `json:"compact_cost_pct,omitempty"`

	// ColdResumeCostCWTokens / ColdResumeCostPct estimate the cost of
	// replaying the conversation prefix when the cache has gone cold —
	// i.e. the floor on what continuing this session will cost if you
	// walked away past the cache TTL. We can't know the next prompt or
	// response size, but the prefix itself is fixed; everything in it has
	// to be re-cached at the cache_create rate. Computed off the most
	// recent turn's view of the context.
	ColdResumeCostCWTokens float64 `json:"cold_resume_cost_cw_tokens,omitempty"`
	ColdResumeCostPct      float64 `json:"cold_resume_cost_pct,omitempty"`

	// TokensPerPctCW echoes the latest median used for the % conversions
	// above. Lets the UI render "(at ≈X tok/1%)" without a second call.
	TokensPerPctCW float64 `json:"tokens_per_pct_cw,omitempty"`

	// Recommendation is "" | "ok" | "watch" | "compact". Reason explains
	// why for the UI tooltip.
	Recommendation       string `json:"recommendation,omitempty"`
	RecommendationReason string `json:"recommendation_reason,omitempty"`
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
	Pct              int     `json:"pct"`
	ResetTSISO       string  `json:"reset_ts,omitempty"`
	WindowStartTSISO string  `json:"window_start_ts,omitempty"`
	TimeToResetMS    int64   `json:"time_to_reset_ms,omitempty"`

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

	// Per-turn-derived insights for up to 5 most-recent sessions.
	// tokens_per_pct_cw drives the % estimates; pulled once and reused
	// across cards for consistency.
	tokensPerPct, _, _, hasCal, _ := s.Store.LatestCalibrationMedian(ctx, "session", 10)
	out.RecentSessions = s.queryRecentSessionInsights(ctx, now, recentWindowS, tokensPerPct, hasCal)

	writeJSON(w, http.StatusOK, out)
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

// cwExpr is the SQL fragment that converts a turn's raw token columns into
// cost-weighted tokens, mirroring the calibrator's weighting (input ×1,
// output ×5, cache_read ×0.1, cache_create_5m ×1.25, cache_create_1h ×2).
const cwExpr = `(input_tokens
                 + output_tokens * 5.0
                 + cache_read * 0.1
                 + cache_create_5m * 1.25
                 + cache_create_1h * 2.0)`
const rawExpr = `(input_tokens + output_tokens + cache_read + cache_create_5m + cache_create_1h)`

// coldPrefixCWExpr is the cost-weighted price of replaying the
// conversation prefix when the cache has gone cold. Bounds the lower
// estimate of "what would resuming this session cost" — we can't predict
// the new prompt or response size, but the prefix itself is fixed and
// must be re-paid as cache creation (assumes the 5m TTL Claude Code uses
// by default; sessions on 1h would skew higher). The prefix at turn N is
// approximated by what the model saw as input + what it produced as
// output during that turn.
const coldPrefixCWExpr = `((input_tokens + cache_read + cache_create_5m + cache_create_1h + output_tokens) * 1.25)`

// queryRecentSessionInsights returns sessions whose last turn falls
// inside [now-windowS, now], each fully decorated with per-turn insights.
// A hard cap of recentSessionsMax keeps the panel from blowing up on days
// where the user has been jumping between many projects.
func (s *Server) queryRecentSessionInsights(ctx context.Context, now time.Time, windowS int, tokensPerPct float64, hasCal bool) []sessionInsight {
	const recentSessionsMax = 10
	cutoffMS := now.UnixMilli() - int64(windowS)*1000
	rows, err := s.Store.DB.QueryContext(ctx, `
		SELECT session_uuid, project, last_ts_unix_ms
		FROM sessions
		WHERE last_ts_unix_ms >= ?
		ORDER BY last_ts_unix_ms DESC LIMIT ?
	`, cutoffMS, recentSessionsMax)
	if err != nil {
		return nil
	}
	defer rows.Close()
	type head struct {
		uuid    string
		project string
		lastMS  int64
	}
	var heads []head
	for rows.Next() {
		var h head
		if err := rows.Scan(&h.uuid, &h.project, &h.lastMS); err != nil {
			return nil
		}
		heads = append(heads, h)
	}
	if len(heads) == 0 {
		return nil
	}

	out := make([]sessionInsight, 0, len(heads))
	for _, h := range heads {
		insight := s.buildSessionInsight(ctx, h.uuid, h.project, h.lastMS, now, tokensPerPct, hasCal)
		if insight != nil {
			out = append(out, *insight)
		}
	}
	return out
}

// buildSessionInsight populates one sessionInsight from the per-turn data
// for a single session. Returns nil only on a hard query failure — partial
// data still produces a card.
func (s *Server) buildSessionInsight(ctx context.Context, uuid, project string, lastMS int64, now time.Time, tokensPerPct float64, hasCal bool) *sessionInsight {
	info := &sessionInsight{
		SessionUUID:  uuid,
		Project:      project,
		LastTSISO:    time.UnixMilli(lastMS).UTC().Format(time.RFC3339),
		LastTSUnixMS: lastMS,
		AgeS:         (now.UnixMilli() - lastMS) / 1000,
	}

	var rawSum sql.NullInt64
	var cwSum sql.NullFloat64
	var turnCount sql.NullInt64
	if err := s.Store.DB.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(`+rawExpr+`), 0), COALESCE(SUM(`+cwExpr+`), 0)
		FROM turns WHERE session_uuid = ?
	`, uuid).Scan(&turnCount, &rawSum, &cwSum); err != nil {
		return info
	}
	info.TurnCount = int(turnCount.Int64)
	info.TotalRawTokens = rawSum.Int64
	info.TotalCWTokens = round2(cwSum.Float64)
	if info.TurnCount > 0 {
		info.SessionAvgCWTokens = round2(cwSum.Float64 / float64(info.TurnCount))
	}

	if rows, err := s.Store.DB.QueryContext(ctx, `
		SELECT `+rawExpr+`, `+cwExpr+`, `+coldPrefixCWExpr+`
		FROM turns WHERE session_uuid = ?
		ORDER BY turn_idx DESC LIMIT 3
	`, uuid); err == nil {
		defer rows.Close()
		var lastRaw int64
		var lastCW, lastColdPrefix, sumCW float64
		var n int
		for rows.Next() {
			var r int64
			var c, cp float64
			if err := rows.Scan(&r, &c, &cp); err != nil {
				break
			}
			if n == 0 {
				lastRaw = r
				lastCW = c
				lastColdPrefix = cp
			}
			sumCW += c
			n++
		}
		info.LastTurnRawTokens = lastRaw
		info.LastTurnCWTokens = round2(lastCW)
		info.ColdResumeCostCWTokens = round2(lastColdPrefix)
		if n > 0 {
			info.Recent3AvgCWTokens = round2(sumCW / float64(n))
		}
	}

	var sinceCompact sql.NullInt64
	if err := s.Store.DB.QueryRowContext(ctx, `
		WITH last_compact AS (
		    SELECT COALESCE(MAX(turn_idx), -1) AS idx
		      FROM turns
		     WHERE session_uuid = ? AND post_compact = 1
		)
		SELECT COUNT(*) FROM turns
		 WHERE session_uuid = ?
		   AND turn_idx > (SELECT idx FROM last_compact)
	`, uuid, uuid).Scan(&sinceCompact); err == nil {
		info.TurnsSinceCompact = int(sinceCompact.Int64)
	}

	if hasCal && tokensPerPct > 0 {
		info.TokensPerPctCW = round2(tokensPerPct)
		info.LastTurnPct = round2(info.LastTurnCWTokens / tokensPerPct)
		info.SessionAvgPct = round2(info.SessionAvgCWTokens / tokensPerPct)
		info.Recent3AvgPct = round2(info.Recent3AvgCWTokens / tokensPerPct)
		info.ColdResumeCostPct = round2(info.ColdResumeCostCWTokens / tokensPerPct)
	}

	if info.SessionAvgCWTokens > 0 && info.TurnCount >= 5 && info.Recent3AvgCWTokens > 0 {
		ratio := info.Recent3AvgCWTokens / info.SessionAvgCWTokens
		switch {
		case ratio >= 2.0:
			info.Recommendation = "compact"
			info.RecommendationReason = "recent turns are >2× the session average — context has likely bloated"
		case ratio >= 1.5:
			info.Recommendation = "watch"
			info.RecommendationReason = "recent turns are running heavier than the session average"
		default:
			info.Recommendation = "ok"
		}
	}

	if (info.Recommendation == "compact" || info.Recommendation == "watch") && info.Recent3AvgCWTokens > 0 {
		info.CompactCostCWTokens = info.Recent3AvgCWTokens
		info.CompactCostPct = info.Recent3AvgPct
	}
	return info
}
