package api

import (
	"net/http"
	"time"
)

// NowResponse is everything the "Now" page needs in one payload.
type NowResponse struct {
	OK        bool          `json:"ok"`
	Session   *windowState  `json:"session"`
	Week      *windowState  `json:"week"`
	LastPoll  *pollSummary  `json:"last_poll"`
	NowMS     int64         `json:"server_now_ms"`
}

type windowState struct {
	Pct             int     `json:"pct"`
	ResetTSISO      string  `json:"reset_ts,omitempty"`     // empty if unparsed
	WindowStartTSISO string `json:"window_start_ts,omitempty"`
	BurnPctPerHour  float64 `json:"burn_pct_per_hour,omitempty"`
	BurnOK          bool    `json:"burn_ok"`
	ETAMS           int64   `json:"eta_ms,omitempty"` // ms from server_now_ms until 100%
	ETAISO          string  `json:"eta_ts,omitempty"`
	ResetDetected   bool    `json:"reset_detected_in_last_obs"`
}

type pollSummary struct {
	TSISO           string `json:"ts"`
	AgeS            int64  `json:"age_s"`
	ParseOK         bool   `json:"parse_ok"`
	ExtractorOrigin string `json:"extractor_origin,omitempty"` // populated by storing alongside, see roadmap
	ElapsedS        float64 `json:"elapsed_s"`
}

func (s *Server) handleNow(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := NowResponse{NowMS: time.Now().UnixMilli()}

	obs, err := s.Store.LatestUsage(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if obs == nil {
		writeJSON(w, http.StatusOK, out) // OK: no data yet
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
		out.Session = buildWindow(*obs.SessionPct, obs.SessionResetTSISO, 5*time.Hour, obs.SessionResetDetected)
		if pts, err := s.Store.SessionPctSinceLastReset(ctx); err == nil {
			if slope, ok := burnRate(pts); ok {
				out.Session.BurnPctPerHour = round2(slope)
				out.Session.BurnOK = true
				if etaMS, ok := etaTo100(*obs.SessionPct, slope); ok {
					out.Session.ETAMS = etaMS
					out.Session.ETAISO = time.UnixMilli(out.NowMS + etaMS).UTC().Format(time.RFC3339)
				}
			}
		}
	}

	if obs.WeekPct != nil {
		out.Week = buildWindow(*obs.WeekPct, obs.WeekResetTSISO, 7*24*time.Hour, obs.WeekResetDetected)
		if pts, err := s.Store.WeekPctSinceLastReset(ctx); err == nil {
			if slope, ok := burnRate(pts); ok {
				out.Week.BurnPctPerHour = round2(slope)
				out.Week.BurnOK = true
				if etaMS, ok := etaTo100(*obs.WeekPct, slope); ok {
					out.Week.ETAMS = etaMS
					out.Week.ETAISO = time.UnixMilli(out.NowMS + etaMS).UTC().Format(time.RFC3339)
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, out)
}

func buildWindow(pct int, resetISO string, span time.Duration, resetDetected bool) *windowState {
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
	return ws
}

func round2(f float64) float64 {
	return float64(int(f*100+0.5)) / 100
}
