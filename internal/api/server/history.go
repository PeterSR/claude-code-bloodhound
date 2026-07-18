package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

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

	// burn_window_min is the burn-rate smoothing baseline. Wider trades
	// responsiveness for a steadier line; burnSeries floors it regardless.
	burnWindowMin := 45
	if v := r.URL.Query().Get("burn_window_min"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 240 {
			burnWindowMin = n
		}
	}

	out := routes.HistoryResponse{OK: true, WindowDays: days}

	rows, err := s.Store.DB.QueryContext(ctx, `
		SELECT ts_unix_ms, session_pct, week_pct,
		       session_saturated, week_saturated,
		       session_reset_detected, week_reset_detected,
		       session_pct_valid, week_pct_valid
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
			svalid, wvalid             int
			sNull, wNull               interface{}
		)
		if err := rows.Scan(&tsMS, &sNull, &wNull, &ssat, &wsat, &sreset, &wreset, &svalid, &wvalid); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if v, ok := nullableInt(sNull); ok {
			spct = &v
		}
		if v, ok := nullableInt(wNull); ok {
			wpct = &v
		}
		out.Observations = append(out.Observations, routes.ObservationPoint{
			TSUnixMS:         tsMS,
			SessionPct:       spct,
			WeekPct:          wpct,
			SessionSaturated: ssat == 1,
			WeekSaturated:    wsat == 1,
			SessionReset:     sreset == 1,
			WeekReset:        wreset == 1,
			SessionValid:     svalid == 1,
			WeekValid:        wvalid == 1,
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

	// Burn rate: the derivative of both usage curves. Computed here rather
	// than in the browser because the corrections that make it honest
	// (never differentiate across a reset, never differentiate a saturated
	// bucket, refuse baselines too short to out-signal /usage's integer
	// rounding) are worth testing.
	out.BurnWindowMin = burnWindowMin
	out.BurnRate = burnRateSeries(out.Observations, int64(burnWindowMin)*60*1000)

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

// burnRateSeries differentiates both usage curves over the same
// observation list, pairing the results by timestamp.
func burnRateSeries(obs []routes.ObservationPoint, lookbackMS int64) []routes.BurnPoint {
	// Empty rather than nil: the client maps over this directly, and a nil
	// slice would serialize to null.
	if len(obs) == 0 {
		return []routes.BurnPoint{}
	}
	// Usable and SegmentBreak come from the store's authoritative
	// reset/misparse flags; the derivative doesn't second-guess them.
	toRatePoints := func(
		pick func(routes.ObservationPoint) *int,
		saturated, valid, reset func(routes.ObservationPoint) bool,
	) []ratePoint {
		pts := make([]ratePoint, len(obs))
		for i, o := range obs {
			v := pick(o)
			pts[i] = ratePoint{
				TSUnixMS:     o.TSUnixMS,
				Usable:       v != nil && !saturated(o) && valid(o),
				SegmentBreak: reset(o),
			}
			if v != nil {
				pts[i].Pct = *v
			}
		}
		return pts
	}

	sess := burnSeries(toRatePoints(
		func(o routes.ObservationPoint) *int { return o.SessionPct },
		func(o routes.ObservationPoint) bool { return o.SessionSaturated },
		func(o routes.ObservationPoint) bool { return o.SessionValid },
		func(o routes.ObservationPoint) bool { return o.SessionReset },
	), lookbackMS)
	week := burnSeries(toRatePoints(
		func(o routes.ObservationPoint) *int { return o.WeekPct },
		func(o routes.ObservationPoint) bool { return o.WeekSaturated },
		func(o routes.ObservationPoint) bool { return o.WeekValid },
		func(o routes.ObservationPoint) bool { return o.WeekReset },
	), lookbackMS)

	out := make([]routes.BurnPoint, 0, len(obs))
	for i, o := range obs {
		p := routes.BurnPoint{TSUnixMS: o.TSUnixMS}
		if sess[i] != nil {
			p.SessionPctHour = ptr(round2(*sess[i]))
		}
		if week[i] != nil {
			p.WeekPctHour = ptr(round2(*week[i]))
		}
		out = append(out, p)
	}
	return out
}

func ptr[T any](v T) *T { return &v }

func convertCalPoints(in []store.CalibrationPoint) []routes.CalibrationOutput {
	out := make([]routes.CalibrationOutput, 0, len(in))
	for _, p := range in {
		out = append(out, routes.CalibrationOutput{
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
