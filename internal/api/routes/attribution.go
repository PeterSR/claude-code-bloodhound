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
	// By is how each window's slices are grouped: "project", "session", or
	// "cwd". All three rollup tables (Projects, Sessions, Cwds) are
	// returned regardless; By only selects which grouping the per-window
	// Slices below use.
	By         string `json:"by"`
	WindowDays int    `json:"window_days"`
	// Window is "current" when the request narrowed everything below to the
	// single open limit window (?window=current), "" otherwise (the normal
	// WindowDays lookback). Echoed back so a caller can tell which scope a
	// given response used without having to remember what it asked for.
	Window string `json:"window,omitempty"`

	// TokensPerPctCW is what one point of this meter cost most recently,
	// reconciled over a whole observed window. This is the rate the
	// estimated fallback runs at. Zero means nothing has been measurable
	// yet, in which case unmeasured spend shows tokens but no percentage.
	TokensPerPctCW float64 `json:"tokens_per_pct_cw,omitempty"`

	// Windows is the per-limit-window breakdown, oldest first.
	Windows []AttrWindow `json:"windows"`
	// Projects, Sessions and Cwds roll the same rows up three ways, biggest
	// first: by the sanitized project directory Claude Code invents, by the
	// session that spent it (a subagent's spend folded into its
	// dispatcher's row), and by the dispatcher's own absolute working
	// directory. Cwds is the finer-grained view Projects can't provide: one
	// project can span several literal directories, so Projects can't say
	// which of them actually spent a given share.
	Projects []AttrGroup `json:"projects"`
	Sessions []AttrGroup `json:"sessions"`
	Cwds     []AttrGroup `json:"cwds"`

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
	// Cwd is the absolute working directory behind Project's sanitized name
	// (project only replaces "/" with "-", which a real path segment can
	// also contain, so it can't be reversed). Populated for a session-keyed
	// group (one owner, one directory) and for a cwd-keyed group (where
	// it's simply the same value as Key); a project-keyed group omits it
	// rather than pick one of the several directories that project name can
	// legitimately span. For a group whose key is a session (or a
	// directory) that a supervisor's subagents ran under, this is the
	// dispatcher's own cwd, never a subagent's, even though the subagent's
	// spend is folded into this same row.
	//
	// Empty when the underlying session's cwd was never captured
	// (transcript rotated off disk before this field existed). In a
	// cwd-keyed group that case has its own explicit key instead, the
	// literal string "__unknown_cwd__", so a caller doesn't have to
	// distinguish "unknown directory" from Key == "" (the unattributed
	// remainder below): those are different facts, not the same gap.
	Cwd string `json:"cwd,omitempty"`

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

	// PerWindow is this group's share of each individual limit window in the
	// range, rather than summed across all of them the way Pct is: the
	// denominator a grant needs is "share of one window", and Pct can't
	// provide it once a range spans more than one (a 5h rollup over --days 7
	// sums roughly 30 windows, so Pct there answers "how much this week",
	// never "how much of my current budget"). Only populated when asked for
	// (CLI --per-window, API ?per_window=1); nil otherwise, same convention
	// as the rest of this response's optional detail.
	PerWindow []AttrGroupWindow `json:"per_window,omitempty"`
}

// AttrGroupWindow is one grouping key's share of a single limit window: the
// per-group element of AttrGroup.PerWindow.
type AttrGroupWindow struct {
	WindowStartUnixMS int64 `json:"window_start_unix_ms"`
	WindowEndUnixMS   int64 `json:"window_end_unix_ms"`
	// InProgress comes straight from limit_windows, never inferred from the
	// wall clock: the 5h window is usage-triggered rather than aligned to a
	// fixed schedule, so "is this the open window" isn't something a caller
	// could derive from the current time on its own.
	InProgress bool `json:"in_progress,omitempty"`

	// Pct is this group's share of THIS window alone, the grant-currency
	// figure PerWindow exists to provide. MeasuredPct / EstimatedPct split
	// it the same way the rest of this file's percentages do.
	Pct          float64 `json:"pct"`
	MeasuredPct  float64 `json:"measured_pct"`
	EstimatedPct float64 `json:"estimated_pct"`

	CWTokens  float64 `json:"cw_tokens"`
	RawTokens int64   `json:"raw_tokens"`
	TurnCount int     `json:"turn_count"`
}

// SessionAttribution is one session's cost against both meters, embedded in
// the session list and the session detail payload.
type SessionAttribution struct {
	// Cwd is the session's absolute working directory: the same value
	// Project would reverse to if project's sanitization were reversible.
	// For a session that dispatched subagents, this is its own cwd, never a
	// subagent's, even though a subagent's spend rolls up into these same
	// totals. Empty when the underlying session's cwd was never captured
	// (transcript rotated off disk before this field existed) - render that
	// as "unknown", not as a blank that reads like a bug.
	Cwd string `json:"cwd,omitempty"`

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
