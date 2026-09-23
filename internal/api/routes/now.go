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

	// AccountID is the Claude account whose meter this is. Each account has
	// its own; see ?account= on the endpoint.
	AccountID int64 `json:"account_id,omitempty"`

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

	// Quota is what the transcripts say about the quota verdict, as
	// opposed to what the percentage implies. Nil when nothing has been
	// refused inside a window that is still closed, which is the ordinary
	// case; a consumer must read nil as "no claim either way" rather than
	// as "not at the cap", since Session.Pct is the field that answers
	// that.
	Quota *NowQuota `json:"quota,omitempty"`
}

// NowQuota is the account's current quota situation as read off the
// transcripts, which is the half a percentage cannot express.
//
// The meter stops moving at 100% in three situations that look identical on
// screen and are nothing alike to work under: spend flowing to the
// pay-per-use tier, requests refused outright, and requests still going
// through at the lower priority Claude Code offers when the five hour
// window closes. UsingOverage, Refused and LowPriorityActive are the three
// answers, and a consumer that renders "at the cap" without consulting them
// is guessing between them.
type NowQuota struct {
	// Refused is whether a request is on record as turned away for quota
	// reasons inside a window that has not reopened.
	Refused      bool   `json:"refused"`
	RefusedTSISO string `json:"refused_ts,omitempty"`

	// Bucket is which window the refusal was about ("session" | "week").
	Bucket string `json:"bucket,omitempty"`

	// ResetTSISO is when that window reopens, taken from the server's own
	// timestamp on the refusal rather than parsed off the /usage panel.
	ResetTSISO string `json:"reset_ts,omitempty"`

	// The pay-per-use tier's verdict at the moment of the refusal.
	// UsingOverage is the one that settles whether "on extra usage" is a
	// true sentence to put on a gauge.
	OverageStatus         string `json:"overage_status,omitempty"`
	OverageDisabledReason string `json:"overage_disabled_reason,omitempty"`
	UsingOverage          bool   `json:"using_overage"`

	// LowPriorityOffered is whether the fallback was on the table;
	// LowPriorityActive whether at least one session took it and the
	// window it stands in for is still closed.
	LowPriorityOffered    bool     `json:"low_priority_offered"`
	LowPriorityActive     bool     `json:"low_priority_active"`
	LowPrioritySinceTSISO string   `json:"low_priority_since_ts,omitempty"`
	LowPriorityUntilTSISO string   `json:"low_priority_until_ts,omitempty"`
	LowPrioritySessions   []string `json:"low_priority_sessions,omitempty"`
}

// Cap states, the values NowWindow.CapState takes. Named because the UI,
// the weaverbird provider and the announcer all branch on them and a typo
// in any one of them would silently pick the wrong wording.
const (
	// CapExtraUsage: past the included quota and billing to the
	// pay-per-use tier. Spend continues, the meter does not move.
	CapExtraUsage = "extra_usage"
	// CapRefused: past the quota with nothing absorbing the overflow.
	// Requests are being turned away and work stops here.
	CapRefused = "refused"
	// CapLowPriority: past the quota and still working, at the lower
	// request priority, against the weekly allowance. Slower, not stopped.
	CapLowPriority = "low_priority"
)

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

	// Stale marks a reading carried forward from an earlier observation
	// because the most recent poll produced no percentage. The number is
	// real, it is just older than LastPoll's timestamp implies, so
	// StaleTSISO carries when it was actually read and consumers render
	// it in their stale treatment rather than as a live gauge.
	//
	// A stale window is deliberately still a window: the last known
	// percentage, marked old, beats nothing at all, which is
	// indistinguishable from "bloodhound has never seen your usage".
	Stale      bool   `json:"stale"`
	StaleTSISO string `json:"stale_ts,omitempty"`

	// CapState says what this window being at its cap actually means right
	// now, from the transcript evidence in NowQuota: one of the Cap*
	// constants above, or "" when nothing has been refused in this window
	// and there is therefore no evidence to interpret Saturated with.
	//
	// Saturated alone was read for a long time as "extra usage is billing",
	// which is one of three possible readings and the wrong one whenever
	// the pay-per-use tier is unavailable. A consumer that has this field
	// should say nothing about billing without it.
	CapState string `json:"cap_state,omitempty"`
}

// NowPoll summarises the freshness of the latest /usage observation.
type NowPoll struct {
	TSISO    string  `json:"ts"`
	AgeS     int64   `json:"age_s"`
	ParseOK  bool    `json:"parse_ok"`
	ElapsedS float64 `json:"elapsed_s"`
}
