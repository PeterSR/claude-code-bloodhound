package routes

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

	// BurnRate is the derivative of the two usage curves: how fast the
	// limit was being consumed at each instant, in percentage points per
	// hour. Smoothed over BurnWindowMin because /usage is integer-
	// quantized and adjacent readings mostly measure rounding.
	BurnRate      []BurnPoint `json:"burn_rate"`
	BurnWindowMin int         `json:"burn_window_min"`
}

// BurnPoint is one instant's rate of limit consumption. Either rate may be
// absent: at the start of a window there is nothing to measure against,
// and while a bucket is saturated its percentage is pinned so the slope
// would read a misleading zero.
type BurnPoint struct {
	TSUnixMS       int64    `json:"ts_unix_ms"`
	SessionPctHour *float64 `json:"session_pct_per_hour,omitempty"`
	WeekPctHour    *float64 `json:"week_pct_per_hour,omitempty"`
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
	// Valid is false for a misparsed reading: a value that fell further
	// than rounding allows and then recovered. Charts should drop it rather
	// than draw the dip and rebound. Defaults true.
	SessionValid bool `json:"session_pct_valid"`
	WeekValid    bool `json:"week_pct_valid"`
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
