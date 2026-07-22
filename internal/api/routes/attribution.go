package routes

// AttributionResponse answers "where did my limit actually go?": the /usage
// meters broken down by the Claude Code session, and by the working
// directory, that moved them. Served at PathAttribution.
//
// Percentages are points of the chosen meter. For bucket "week" they add up
// cleanly: every session in a weekly window sums to that window's usage. For
// bucket "5h" a session that ran across several windows can exceed 100: read
// its total as "1.4 five-hour budgets" and its peak as the worst single one.
type AttributionResponse struct {
	OK     bool   `json:"ok"`
	Bucket string `json:"bucket"` // "week" | "5h"
	// By is how each window's slices are grouped: "project" or "session".
	// Both rollup tables are returned regardless.
	By         string `json:"by"`
	WindowDays int    `json:"window_days"`

	// TokensPerPctCW is what one point of this meter cost most recently,
	// reconciled over a whole observed window. This is the rate the
	// estimated fallback runs at. Zero means nothing has been measurable
	// yet, in which case unmeasured spend shows tokens but no percentage.
	TokensPerPctCW float64 `json:"tokens_per_pct_cw,omitempty"`

	// Windows is the per-limit-window breakdown, oldest first.
	Windows []AttrWindow `json:"windows"`
	// Projects and Sessions roll the same rows up two ways, biggest first.
	Projects []AttrGroup `json:"projects"`
	Sessions []AttrGroup `json:"sessions"`

	// Totals across the requested range.
	TotalPct        float64 `json:"total_pct"`
	MeasuredPct     float64 `json:"measured_pct"`
	EstimatedPct    float64 `json:"estimated_pct"`
	UnattributedPct float64 `json:"unattributed_pct"`
}

// AttrWindow is one limit window and the slices that filled it.
type AttrWindow struct {
	StartUnixMS int64 `json:"start_unix_ms"`
	EndUnixMS   int64 `json:"end_unix_ms"`
	ResetUnixMS int64 `json:"reset_unix_ms,omitempty"`

	// Inferred marks a window synthesized on a fixed grid because no /usage
	// reading covered it: everything in it is estimated. Partial marks the
	// oldest observed window, joined mid-flight, so its total under-counts.
	// InProgress marks the newest, still filling.
	Inferred   bool `json:"inferred,omitempty"`
	Partial    bool `json:"partial,omitempty"`
	InProgress bool `json:"in_progress,omitempty"`
	HitCap     bool `json:"hit_cap,omitempty"`

	// MeasuredPct is how far the meter was observed to move inside this
	// window; AttributedPct is the sum of the slices below, which also
	// includes estimated spend the meter had not yet reported.
	MeasuredPct   float64 `json:"measured_pct"`
	AttributedPct float64 `json:"attributed_pct"`
	PeakPct       int     `json:"peak_pct,omitempty"`

	Slices []AttrSlice `json:"slices"`
}

// AttrSlice is one project's or one session's share of a single window.
// Key is "" for the unattributed remainder: meter movement no ingested turn
// accounts for, most often a second machine on the same account.
type AttrSlice struct {
	Key          string  `json:"key"`
	Label        string  `json:"label"`
	Pct          float64 `json:"pct"`
	MeasuredPct  float64 `json:"measured_pct"`
	EstimatedPct float64 `json:"estimated_pct"`
	CWTokens     float64 `json:"cw_tokens"`
	TurnCount    int     `json:"turn_count"`
}

// AttrGroup is one project's or one session's total across every window in
// the range.
type AttrGroup struct {
	Key     string `json:"key"`
	Project string `json:"project,omitempty"`

	Pct          float64 `json:"pct"`
	MeasuredPct  float64 `json:"measured_pct"`
	EstimatedPct float64 `json:"estimated_pct"`
	// PeakPct is the largest share taken in any single window: the number
	// that stays bounded by 100 when Pct spans several.
	PeakPct float64 `json:"peak_pct"`
	// Share is this group's fraction of everything attributed in the range.
	Share float64 `json:"share"`

	CWTokens  float64 `json:"cw_tokens"`
	RawTokens int64   `json:"raw_tokens"`
	TurnCount int     `json:"turn_count"`
	Sessions  int     `json:"sessions"`
	Windows   int     `json:"windows"`

	FirstTSUnixMS int64 `json:"first_ts_unix_ms,omitempty"`
	LastTSUnixMS  int64 `json:"last_ts_unix_ms,omitempty"`
}

// SessionAttribution is one session's cost against both meters, embedded in
// the session list and the session detail payload.
type SessionAttribution struct {
	// WeekPct is the session's share of the weekly limit: the headline
	// number, since weekly windows are long enough that most sessions sit
	// inside exactly one.
	WeekPct float64 `json:"week_pct"`
	// FiveHPct sums the session's share across every 5h window it spanned,
	// so it exceeds 100 for a session longer than one window. FiveHPeakPct
	// is its worst single window.
	FiveHPct     float64 `json:"five_h_pct"`
	FiveHPeakPct float64 `json:"five_h_peak_pct"`

	// MeasuredPct / EstimatedPct split WeekPct by provenance: measured came
	// from observed meter movement, estimated from the calibration median.
	MeasuredPct  float64 `json:"measured_pct"`
	EstimatedPct float64 `json:"estimated_pct"`

	// Windows5h / WindowsWeek are how many limit windows the session touched.
	Windows5h   int `json:"windows_5h,omitempty"`
	WindowsWeek int `json:"windows_week,omitempty"`
}

// SessionAttributionDetail adds the per-window walk to the totals: how one
// conversation's cost accumulated over time.
type SessionAttributionDetail struct {
	SessionAttribution
	Week []SessionWindowSlice `json:"week"`
	FivH []SessionWindowSlice `json:"five_h"`
}

// SessionWindowSlice is one session's share of one window, with enough
// window context to render it without a second lookup.
type SessionWindowSlice struct {
	WindowStartUnixMS int64 `json:"window_start_unix_ms"`
	WindowEndUnixMS   int64 `json:"window_end_unix_ms"`
	Inferred          bool  `json:"inferred,omitempty"`
	Partial           bool  `json:"partial,omitempty"`
	InProgress        bool  `json:"in_progress,omitempty"`

	Pct          float64 `json:"pct"`
	MeasuredPct  float64 `json:"measured_pct"`
	EstimatedPct float64 `json:"estimated_pct"`
	// WindowPct is everything attributed in the same window, so the UI can
	// show this session's share against its neighbours'.
	WindowPct float64 `json:"window_pct"`

	CWTokens      float64 `json:"cw_tokens"`
	RawTokens     int64   `json:"raw_tokens"`
	TurnCount     int     `json:"turn_count"`
	FirstTSUnixMS int64   `json:"first_ts_unix_ms,omitempty"`
	LastTSUnixMS  int64   `json:"last_ts_unix_ms,omitempty"`
}
