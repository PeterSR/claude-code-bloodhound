package routes

import (
	"github.com/PeterSR/claude-code-bloodhound/internal/sessioninsight"
)

// NowResponse is everything the "Now" page needs in one payload.
type NowResponse struct {
	OK       bool       `json:"ok"`
	Session  *NowWindow `json:"session"`
	Week     *NowWindow `json:"week"`
	LastPoll *NowPoll   `json:"last_poll"`
	NowMS    int64      `json:"server_now_ms"`

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
	SessionHistory []NowHistoryPoint `json:"session_history,omitempty"`
	WeekHistory    []NowHistoryPoint `json:"week_history,omitempty"`

	// RecentSessions is up to 5 most-recent sessions, each with full
	// per-turn insights (last-turn cost, recent-3 vs session-average,
	// compaction recommendation). Trailing entries that are far older
	// than the cluster are dropped so a stale list doesn't pad out the
	// panel.
	RecentSessions []sessioninsight.Insight `json:"recent_sessions,omitempty"`
}

// NowHistoryPoint is one observation slimmed for the in-window chart.
type NowHistoryPoint struct {
	TSUnixMS  int64 `json:"ts_unix_ms"`
	Pct       *int  `json:"pct,omitempty"`
	Saturated bool  `json:"saturated"`
}

// NowWindow describes one bucket. Two distinct time concepts to keep
// straight:
//
//   - Reset (always shown when known): the natural cycle boundary parsed
//     from /usage. "We are inside [window_start_ts, reset_ts]."
//   - Limit (only shown when burn-rate projection says we'd hit 100% before
//     reset): the actionable warning. Hidden otherwise — projecting "100%
//     in 8 days" when the bucket resets in 3 hours adds noise, not signal.
type NowWindow struct {
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

// NowPoll summarises the freshness of the latest /usage observation.
type NowPoll struct {
	TSISO    string  `json:"ts"`
	AgeS     int64   `json:"age_s"`
	ParseOK  bool    `json:"parse_ok"`
	ElapsedS float64 `json:"elapsed_s"`
}
