package routes

// CapacityResponse answers "how many typical 5h work sessions fit inside my
// weekly limit?" It reconstructs sessions from the /usage percentage series
// and measures how much of the weekly limit each one consumed. Served at
// PathCapacity and rendered as the "Weekly capacity" section on History.
type CapacityResponse struct {
	OK          bool `json:"ok"`
	WindowWeeks int  `json:"window_weeks"`

	// Weeks is the per-weekly-window breakdown, oldest first. Each week is a
	// stack of session slices summing toward the 100% weekly cap.
	Weeks []WeekCapacity `json:"weeks"`

	// Summary over complete work sessions, those bounded by observed session
	// resets that moved the weekly meter past the work-session floor (a brief
	// check-in is not a work session). Zero/omitted while SessionCount is
	// below the minimum needed for the median to mean anything.
	TypicalSessionWeekPct float64 `json:"typical_session_week_pct,omitempty"` // median
	SessionWeekPctP25     float64 `json:"session_week_pct_p25,omitempty"`
	SessionWeekPctP75     float64 `json:"session_week_pct_p75,omitempty"`
	SessionsPerWeek       float64 `json:"sessions_per_week,omitempty"`
	DaysPerSession        float64 `json:"days_per_session,omitempty"`
	SessionCount          int     `json:"session_count"`

	// MaxedSessionsPerWeek is the ceiling from the calibration ratio: how many
	// sessions would fit if every one maxed out its 5h limit. Secondary
	// reference only; most sessions are nowhere near maxed. Omitted unless
	// both buckets have a calibration median.
	MaxedSessionsPerWeek float64 `json:"maxed_sessions_per_week,omitempty"`
}

// WeekCapacity is one weekly limit window and the sessions that filled it.
type WeekCapacity struct {
	StartUnixMS int64 `json:"start_unix_ms"`
	// ResetUnixMS is the week_reset that closed this window. Absent on the
	// most recent (still-running) week.
	ResetUnixMS  int64          `json:"reset_unix_ms,omitempty"`
	Sessions     []SessionSlice `json:"sessions"`
	TotalWeekPct float64        `json:"total_week_pct"`
	// HitCap is true if any reading in the week was saturated (>=99%).
	HitCap bool `json:"hit_cap"`
	// InProgress marks the most recent week: it has not reset yet, so its
	// total is still climbing.
	InProgress bool `json:"in_progress,omitempty"`
	// Partial marks the oldest week: the series started mid-window, so its
	// total under-counts what was actually spent before collection began.
	Partial bool `json:"partial,omitempty"`
}

// SessionSlice is one work session's contribution within one week. A session
// that straddles a weekly reset produces one slice in each week it touched.
type SessionSlice struct {
	StartUnixMS int64   `json:"start_unix_ms"`
	EndUnixMS   int64   `json:"end_unix_ms"`
	WeekPct     float64 `json:"week_pct"`
}
