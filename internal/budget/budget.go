// Package budget models what one working directory is allowed to spend against
// a limit bucket, and decides how much pressure it is under right now.
//
// Absorbed from claude-usage-governor. What came across is the budget model and
// the lease mechanism; what did not is that tool's wall guard, because
// bloodhound already has one. The 5h cliff is watched by the limit_projection,
// threshold and saturation sensors, which is the always-on pressure that
// applies whether or not a directory has a budget. Porting a second projection
// alongside them would have given two answers to one question.
//
// The vocabulary is bloodhound's, not the governor's, and bloodhound uses two
// spellings for the same two buckets depending on the layer. nowstate, the
// event log and the gauges say "session" and "week"; internal/attribute and
// the attribution API say "5h" and "week". A budget is stored and reported in
// the first spelling, because the event log is where its pressure surfaces,
// and AttributeBucket converts at the one boundary where the second is needed.
// Leaving that conversion implicit is how a budget silently measures nothing:
// GroupAttribution given "session" matches no window at all.
//
// A budget is keyed by working directory, never by project, because this repo
// has measured 140 distinct directories collapsing into 44 project names.
package budget

import (
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Buckets a budget can be set against, spelled the way the rest of bloodhound
// spells them.
const (
	BucketSession = "session"
	BucketWeek    = "week"
)

// Lease kinds. A budget is in force while at least one of its leases holds.
const (
	// LeaseManual never expires on its own. This is what a perpetual budget
	// holds, so that "no leases" can unambiguously mean retired.
	LeaseManual = "manual"
	// LeaseDeadline expires at a wall-clock instant.
	LeaseDeadline = "deadline"
	// LeaseWindowReset expires when the limit window it was bound to turns
	// over.
	LeaseWindowReset = "window_reset"
)

// Why a budget was retired.
const (
	RetiredRevoked = "revoked"
	RetiredExpired = "expired"
	// RetiredReplaced marks a budget displaced by a newer one for the same
	// directory and bucket. Distinct from revoked because the directory is
	// still governed, which is what a reader of the archive wants to know.
	RetiredReplaced = "replaced"
)

// Pressure states, in increasing severity. These are level states in the event
// log, so they describe the world rather than instruct the reader: the reader
// decides what to do about it.
const (
	// StateClear: inside the allowance, with room.
	StateClear = "clear"
	// StateTight: most of the allowance is gone. Worth knowing before starting
	// something large, not worth interrupting for.
	StateTight = "tight"
	// StateExceeded: the allowance is used up. A budget is advisory, so this
	// still describes rather than enforces.
	StateExceeded = "exceeded"
)

// TightFrac is the share of an allowance that must be consumed before pressure
// reads tight. Three quarters leaves enough runway that hearing about it is
// still actionable; warning earlier trains the reader to ignore it.
const TightFrac = 0.75

// Budget is one directory's allowance against one bucket.
type Budget struct {
	ID     int64  `json:"id"`
	Cwd    string `json:"cwd"`
	Bucket string `json:"bucket"`

	// SpendPct is how many points of the meter's movement this directory may
	// itself cause, measured by attribution against the current window. Time
	// spent paused costs nothing because nothing is attributed to it. Zero
	// means the rule is not set.
	SpendPct float64 `json:"spend_pct,omitempty"`

	// MeterPct is a reading on the shared meter to stop at, whoever moved it.
	// It needs no attribution, only the meter. What it buys is headroom at the
	// top for work nobody governed. Zero means the rule is not set.
	MeterPct float64 `json:"meter_pct,omitempty"`

	Note       string `json:"note,omitempty"`
	SetMS      int64  `json:"set_ms"`
	RetiredMS  *int64 `json:"retired_ms,omitempty"`
	RetiredWhy string `json:"retired_why,omitempty"`

	Leases []Lease `json:"leases,omitempty"`
}

// Lease is one reason a budget is still in force.
type Lease struct {
	ID   int64  `json:"id"`
	Kind string `json:"kind"`

	// AtMS carries LeaseDeadline.
	AtMS *int64 `json:"at_ms,omitempty"`

	// Bucket, BoundAfterMS and WindowEndMS carry LeaseWindowReset.
	//
	// WindowEndMS is the end of the window open when the lease was bound, and
	// is absent when no window was open then. That distinction is the whole of
	// this lease kind. With a window open its end is already fixed, so expiry
	// is clock arithmetic needing no meter. With none open there is nothing to
	// attach to yet, because the session window is usage-triggered and the next
	// begins whenever work next begins; expiry then waits for a window to open
	// after BoundAfterMS and close.
	Bucket       string `json:"bucket,omitempty"`
	BoundAfterMS *int64 `json:"bound_after_ms,omitempty"`
	WindowEndMS  *int64 `json:"window_end_ms,omitempty"`

	ExpiredMS *int64 `json:"expired_ms,omitempty"`
}

// HasSpend reports whether the spend rule is set.
func (b Budget) HasSpend() bool { return b.SpendPct > 0 }

// HasMeter reports whether the meter rule is set.
func (b Budget) HasMeter() bool { return b.MeterPct > 0 }

// Live reports whether the budget is still in force, which is structural: at
// least one unexpired lease.
func (b Budget) Live() bool {
	if b.RetiredMS != nil {
		return false
	}
	for _, l := range b.Leases {
		if l.ExpiredMS == nil {
			return true
		}
	}
	return false
}

// Window is the state of one limit window, supplied by the caller so this
// package stays free of the store.
type Window struct {
	// Pct is the meter reading for the bucket, 0 to 100.
	Pct int
	// PctKnown is false when there is no usable reading, which is different
	// from a reading of zero and must not be treated as headroom.
	PctKnown bool
	// AttributedPct is how much of this window's movement the directory caused.
	AttributedPct float64
	// AttributedKnown is false when attribution has not been computed for this
	// window, so a spend rule cannot be evaluated.
	AttributedKnown bool
	// EndMS is when the window turns over by itself, zero when unknown.
	EndMS int64
}

// Pressure is what a budget is under right now.
type Pressure struct {
	Budget Budget `json:"budget"`

	// State is StateClear, StateTight or StateExceeded, or "" when the inputs
	// could not support an answer. Empty is recorded as unknown by the event
	// log rather than silently read as clear, which is the difference between
	// "there is room" and "we cannot see".
	State string `json:"state,omitempty"`

	// Rule names which of the two produced the state: "spend" or "meter".
	// Empty when the state is clear or unknown.
	Rule string `json:"rule,omitempty"`

	// SpentPct and RemainingPct describe the spend rule and are meaningless
	// when only a meter rule is set.
	SpentPct     float64 `json:"spent_pct,omitempty"`
	RemainingPct float64 `json:"remaining_pct,omitempty"`

	// MeterPct is the shared reading the meter rule watches.
	MeterPct int `json:"meter_pct,omitempty"`

	// Reason is one sentence of plain prose, suitable for delivering to a
	// session as-is.
	Reason string `json:"reason,omitempty"`
}

// Evaluate decides the pressure a budget is under against one window.
//
// Severity wins: with both rules set, whichever is further along decides, and
// Rule says which. Data quality comes before either, because a confident
// answer off a reading that does not exist is worse than no answer.
func Evaluate(b Budget, w Window) Pressure {
	p := Pressure{Budget: b, MeterPct: w.Pct}

	spendState, spendReason := evalSpend(b, w)
	meterState, meterReason := evalMeter(b, w)

	if b.HasSpend() && w.AttributedKnown {
		p.SpentPct = round2(w.AttributedPct)
		p.RemainingPct = round2(math.Max(0, b.SpendPct-w.AttributedPct))
	}

	switch {
	case severity(spendState) >= severity(meterState) && spendState != "":
		p.State, p.Rule, p.Reason = spendState, "spend", spendReason
	case meterState != "":
		p.State, p.Rule, p.Reason = meterState, "meter", meterReason
	default:
		p.State = ""
		return p
	}
	if p.State == StateClear {
		p.Rule = ""
	}
	return p
}

func evalSpend(b Budget, w Window) (state, reason string) {
	if !b.HasSpend() {
		return "", ""
	}
	if !w.AttributedKnown {
		// Attribution has not run for this window. Saying "clear" here would
		// be inventing headroom out of a missing measurement.
		return "", ""
	}
	used, allowed := w.AttributedPct, b.SpendPct
	switch {
	case used >= allowed:
		return StateExceeded, fmt.Sprintf(
			"%s has caused %.1f%% of the %s meter against an allowance of %.0f%%",
			Label(b.Cwd), used, BucketLabel(b.Bucket), allowed)
	case used >= allowed*TightFrac:
		return StateTight, fmt.Sprintf(
			"%s has %.1f%% of its %.0f%% %s allowance left",
			Label(b.Cwd), allowed-used, allowed, BucketLabel(b.Bucket))
	default:
		return StateClear, ""
	}
}

func evalMeter(b Budget, w Window) (state, reason string) {
	if !b.HasMeter() {
		return "", ""
	}
	if !w.PctKnown {
		return "", ""
	}
	pct, stop := float64(w.Pct), b.MeterPct
	switch {
	case pct >= stop:
		return StateExceeded, fmt.Sprintf(
			"the %s meter reads %d%%, at or past the %.0f%% %s stops at",
			BucketLabel(b.Bucket), w.Pct, stop, Label(b.Cwd))
	case pct >= stop*TightFrac:
		return StateTight, fmt.Sprintf(
			"the %s meter reads %d%% and %s stops at %.0f%%",
			BucketLabel(b.Bucket), w.Pct, Label(b.Cwd), stop)
	default:
		return StateClear, ""
	}
}

func severity(state string) int {
	switch state {
	case StateExceeded:
		return 3
	case StateTight:
		return 2
	case StateClear:
		return 1
	default:
		return 0
	}
}

// BucketLabel renders a bucket for prose. "session" is bloodhound's name for
// the five-hour window and reads oddly mid-sentence, so prose says "5h".
func BucketLabel(bucket string) string {
	if bucket == BucketSession {
		return "5h"
	}
	return bucket
}

// Label shortens a working directory to its last element for prose, which is
// what makes a one-line warning readable. The full path stays the key.
func Label(cwd string) string {
	if cwd == "" {
		return "this directory"
	}
	return filepath.Base(cwd)
}

// NormalizeCwd makes a directory usable as a key: absolute, cleaned, and with
// any trailing separator removed, so the same directory named two ways does
// not become two budgets.
func NormalizeCwd(cwd string) (string, error) {
	if strings.TrimSpace(cwd) == "" {
		return "", fmt.Errorf("empty working directory")
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", cwd, err)
	}
	return filepath.Clean(abs), nil
}

// AttributeBucket converts a budget's bucket to the spelling
// internal/attribute and the attribution store use. The two layers disagree
// (see the package doc) and this is the only place that knows it.
func AttributeBucket(bucket string) string {
	if bucket == BucketSession {
		return "5h"
	}
	return bucket
}

// ValidBucket reports whether a bucket name is one bloodhound knows.
func ValidBucket(bucket string) bool {
	return bucket == BucketSession || bucket == BucketWeek
}

// Validate checks a budget is meaningful before it is stored.
func Validate(b Budget) error {
	if !ValidBucket(b.Bucket) {
		return fmt.Errorf("bucket must be %q or %q, got %q", BucketSession, BucketWeek, b.Bucket)
	}
	if !b.HasSpend() && !b.HasMeter() {
		return fmt.Errorf("a budget needs a spend rule, a meter rule, or both")
	}
	for _, v := range []struct {
		name string
		pct  float64
	}{{"spend", b.SpendPct}, {"meter", b.MeterPct}} {
		if v.pct < 0 || v.pct > 100 {
			return fmt.Errorf("%s must be between 0 and 100, got %g", v.name, v.pct)
		}
	}
	if len(b.Leases) == 0 {
		return fmt.Errorf("a budget needs at least one lease, or it is retired on arrival")
	}
	return nil
}

// SettleLeases expires every lease that has run out as of now and reports
// whether the budget is still in force. It mutates the passed budget's leases.
//
// windowEnd resolves a window_reset lease that was bound with no window open:
// it is the end of the bucket's current window, or zero when there still is
// not one.
func SettleLeases(b *Budget, now time.Time, windowEnd map[string]int64) (live bool) {
	nowMS := now.UnixMilli()
	for i := range b.Leases {
		l := &b.Leases[i]
		if l.ExpiredMS != nil {
			continue
		}
		if leaseRunOut(*l, nowMS, windowEnd) {
			ms := nowMS
			l.ExpiredMS = &ms
			continue
		}
		live = true
	}
	return live
}

func leaseRunOut(l Lease, nowMS int64, windowEnd map[string]int64) bool {
	switch l.Kind {
	case LeaseManual:
		return false
	case LeaseDeadline:
		return l.AtMS != nil && nowMS >= *l.AtMS
	case LeaseWindowReset:
		if l.WindowEndMS != nil {
			return nowMS >= *l.WindowEndMS
		}
		// Bound with no window open. Wait for one to open after the binding
		// instant and then close. Until such a window exists the lease holds,
		// which is the correct reading of "until the next window resets" when
		// the next window has not started.
		end, ok := windowEnd[l.Bucket]
		if !ok || end == 0 {
			return false
		}
		if l.BoundAfterMS != nil && end <= *l.BoundAfterMS {
			return false
		}
		return nowMS >= end
	default:
		return false
	}
}

// SortBudgets orders budgets for display: by directory, then bucket, so a
// listing is stable across runs.
func SortBudgets(bs []Budget) {
	sort.SliceStable(bs, func(i, j int) bool {
		if bs[i].Cwd != bs[j].Cwd {
			return bs[i].Cwd < bs[j].Cwd
		}
		return bs[i].Bucket < bs[j].Bucket
	})
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }
