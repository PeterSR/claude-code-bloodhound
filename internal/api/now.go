package api

import (
	"context"
	"net/http"
	"time"
)

// NowResponse is everything the "Now" page needs in one payload.
type NowResponse struct {
	OK       bool         `json:"ok"`
	Session  *windowState `json:"session"`
	Week     *windowState `json:"week"`
	LastPoll *pollSummary `json:"last_poll"`
	NowMS    int64        `json:"server_now_ms"`
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
		fillBurn(ctx, s.Store, ws, *obs.SessionPct, true, now)
		out.Session = ws
	}

	if obs.WeekPct != nil {
		ws := buildWindow(*obs.WeekPct, obs.WeekResetTSISO, 7*24*time.Hour, obs.WeekResetDetected, now)
		fillBurn(ctx, s.Store, ws, *obs.WeekPct, false, now)
		out.Week = ws
	}

	writeJSON(w, http.StatusOK, out)
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
