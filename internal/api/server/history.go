package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

// HistoryResponse is what the History page consumes. We return three
// parallel arrays of (ts_unix_ms, value) tuples — easier to feed straight
// into uPlot than parallel-array JSON would be in the API itself.
type HistoryResponse struct {
	OK                    bool                `json:"ok"`
	WindowDays            int                 `json:"window_days"`
	Observations          []ObservationPoint  `json:"observations"`
	SessionCalibration    []CalibrationOutput `json:"session_calibration"`
	WeekCalibration       []CalibrationOutput `json:"week_calibration"`
	LatestSessionMedianCW float64             `json:"latest_session_median_cw,omitempty"`
	LatestWeekMedianCW    float64             `json:"latest_week_median_cw,omitempty"`
	LatestSessionMedianN  int                 `json:"latest_session_median_n"`
	LatestWeekMedianN     int                 `json:"latest_week_median_n"`
	HourlyHeatmap         [7][24]int64        `json:"hourly_heatmap"` // raw tokens per (weekday Sun=0, hour)
	// PctBurnedHeatmap sums positive session_pct deltas per (weekday Sun=0,
	// hour) over the window. Saturated and reset-bracketed deltas are
	// skipped — they don't represent real "burn." Useful next to
	// HourlyHeatmap to see when the user actually eats into their limit
	// vs when raw token volume happens to be high (cache-heavy windows
	// can spend tokens cheaply in pct terms, and vice versa).
	PctBurnedHeatmap [7][24]int64 `json:"pct_burned_heatmap"`
}

// ObservationPoint is a slimmed-down /usage observation for charting.
type ObservationPoint struct {
	TSUnixMS         int64 `json:"ts_unix_ms"`
	SessionPct       *int  `json:"session_pct,omitempty"`
	WeekPct          *int  `json:"week_pct,omitempty"`
	SessionSaturated bool  `json:"session_saturated"`
	WeekSaturated    bool  `json:"week_saturated"`
	SessionReset     bool  `json:"session_reset_detected"`
	WeekReset        bool  `json:"week_reset_detected"`
}

// CalibrationOutput is one calibration point in API form.
type CalibrationOutput struct {
	BTSUnixMS       int64   `json:"b_ts_unix_ms"`
	DeltaPct        int     `json:"delta_pct"`
	GapS            float64 `json:"gap_s"`
	RawTokens       int64   `json:"raw_tokens"`
	CWTokens        float64 `json:"cw_tokens"`
	TurnCount       int     `json:"turn_count"`
	TokensPerPctRaw float64 `json:"tokens_per_pct_raw"`
	TokensPerPctCW  float64 `json:"tokens_per_pct_cw"`
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// window_days controls how far back observations go. Default 7.
	days := 7
	if v := r.URL.Query().Get("window_days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 90 {
			days = n
		}
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()

	out := HistoryResponse{OK: true, WindowDays: days}

	rows, err := s.Store.DB.QueryContext(ctx, `
		SELECT ts_unix_ms, session_pct, week_pct,
		       session_saturated, week_saturated,
		       session_reset_detected, week_reset_detected
		FROM usage_observations
		WHERE ts_unix_ms >= ?
		ORDER BY ts_unix_ms ASC
	`, cutoff)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer rows.Close()
	for rows.Next() {
		var (
			tsMS                       int64
			spct, wpct                 *int
			ssat, wsat, sreset, wreset int
			sNull, wNull               interface{}
		)
		if err := rows.Scan(&tsMS, &sNull, &wNull, &ssat, &wsat, &sreset, &wreset); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if v, ok := nullableInt(sNull); ok {
			spct = &v
		}
		if v, ok := nullableInt(wNull); ok {
			wpct = &v
		}
		out.Observations = append(out.Observations, ObservationPoint{
			TSUnixMS:         tsMS,
			SessionPct:       spct,
			WeekPct:          wpct,
			SessionSaturated: ssat == 1,
			WeekSaturated:    wsat == 1,
			SessionReset:     sreset == 1,
			WeekReset:        wreset == 1,
		})
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	// Pct-burned heatmap from session_pct deltas across adjacent
	// observations. Skipped if either side is saturated (pct doesn't
	// move while capped) or b is right after a reset (rolled back to 0).
	for i := 1; i < len(out.Observations); i++ {
		a, b := out.Observations[i-1], out.Observations[i]
		if a.SessionPct == nil || b.SessionPct == nil {
			continue
		}
		if a.SessionSaturated || b.SessionSaturated {
			continue
		}
		if b.SessionReset {
			continue
		}
		delta := *b.SessionPct - *a.SessionPct
		if delta <= 0 {
			continue
		}
		t := time.UnixMilli(b.TSUnixMS)
		out.PctBurnedHeatmap[t.Weekday()][t.Hour()] += int64(delta)
	}

	sessPts, err := s.Store.CalibrationPoints(ctx, "session")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	out.SessionCalibration = convertCalPoints(sessPts)
	weekPts, err := s.Store.CalibrationPoints(ctx, "week")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	out.WeekCalibration = convertCalPoints(weekPts)

	if cw, _, n, ok, _ := s.Store.LatestCalibrationMedian(ctx, "session", 10); ok {
		out.LatestSessionMedianCW = round2(cw)
		out.LatestSessionMedianN = n
	}
	if cw, _, n, ok, _ := s.Store.LatestCalibrationMedian(ctx, "week", 10); ok {
		out.LatestWeekMedianCW = round2(cw)
		out.LatestWeekMedianN = n
	}

	// Heatmap: raw tokens by (weekday, hour) from `turns` over the window.
	hrows, err := s.Store.DB.QueryContext(ctx, `
		SELECT ts_unix_ms,
		       (input_tokens + output_tokens + cache_read +
		        cache_create_5m + cache_create_1h) AS raw
		FROM turns
		WHERE ts_unix_ms >= ?
	`, cutoff)
	if err == nil {
		defer hrows.Close()
		for hrows.Next() {
			var tsMS, raw int64
			if err := hrows.Scan(&tsMS, &raw); err != nil {
				break
			}
			t := time.UnixMilli(tsMS)
			out.HourlyHeatmap[t.Weekday()][t.Hour()] += raw
		}
	}

	writeJSON(w, http.StatusOK, out)
}

func convertCalPoints(in []store.CalibrationPoint) []CalibrationOutput {
	out := make([]CalibrationOutput, 0, len(in))
	for _, p := range in {
		out = append(out, CalibrationOutput{
			BTSUnixMS:       p.BTSUnixMS,
			DeltaPct:        p.DeltaPct,
			GapS:            round2(p.GapS),
			RawTokens:       p.RawTokens,
			CWTokens:        round2(p.CostWeightedTokens),
			TurnCount:       p.TurnCount,
			TokensPerPctRaw: round2(p.TokensPerPctRaw),
			TokensPerPctCW:  round2(p.TokensPerPctCW),
		})
	}
	return out
}

// nullableInt unboxes a *int64 that came back from sql.QueryContext via
// interface{} — Go's database/sql returns nil for NULL.
func nullableInt(v interface{}) (int, bool) {
	if v == nil {
		return 0, false
	}
	switch x := v.(type) {
	case int64:
		return int(x), true
	case int:
		return x, true
	}
	return 0, false
}
