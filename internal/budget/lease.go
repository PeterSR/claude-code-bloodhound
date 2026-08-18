package budget

import (
	"fmt"
	"strings"
	"time"
)

// ParseUntil turns the --until spelling into a lease.
//
// A budget you know will evaporate is much easier to set than one you have to
// remember to revoke, so the spellings are deliberately cheap to type:
//
//	(empty)      until revoked by hand
//	reset        until this bucket's window turns over
//	session      until the 5h window turns over, whatever bucket this budget is
//	week         until the weekly window turns over
//	2h, 90m      until that long from now
//	18:00        until that time today, or tomorrow if it has already passed
//	2026-08-20   until midnight starting that day
//
// bucket is the budget's own bucket, used by "reset". now anchors every
// relative form. windowEnd gives the end of each bucket's currently open
// window, empty for a bucket with none open, which is the case the
// LeaseWindowReset comment explains.
func ParseUntil(spec, bucket string, now time.Time, windowEnd map[string]int64) (Lease, error) {
	spec = strings.TrimSpace(strings.ToLower(spec))
	nowMS := now.UnixMilli()

	if spec == "" || spec == "revoked" {
		return Lease{Kind: LeaseManual}, nil
	}

	// Window-reset forms.
	target := ""
	switch spec {
	case "reset":
		target = bucket
	case "session", "5h", "session-reset", "5h-reset":
		target = BucketSession
	case "week", "week-reset":
		target = BucketWeek
	}
	if target != "" {
		if !ValidBucket(target) {
			return Lease{}, fmt.Errorf("cannot resolve --until %q: %q is not a bucket", spec, target)
		}
		l := Lease{Kind: LeaseWindowReset, Bucket: target}
		bound := nowMS
		l.BoundAfterMS = &bound
		if end, ok := windowEnd[target]; ok && end > nowMS {
			e := end
			l.WindowEndMS = &e
		}
		return l, nil
	}

	at, err := parseInstant(spec, now)
	if err != nil {
		return Lease{}, err
	}
	if !at.After(now) {
		return Lease{}, fmt.Errorf("--until %q resolves to %s, which has already passed",
			spec, at.Format(time.RFC3339))
	}
	ms := at.UnixMilli()
	return Lease{Kind: LeaseDeadline, AtMS: &ms}, nil
}

func parseInstant(spec string, now time.Time) (time.Time, error) {
	// A duration: 2h, 90m, 45s, 1h30m.
	if d, err := time.ParseDuration(spec); err == nil {
		if d <= 0 {
			return time.Time{}, fmt.Errorf("--until %q is not a future duration", spec)
		}
		return now.Add(d), nil
	}

	// A clock time today, rolling to tomorrow when it has passed. Seconds are
	// accepted so a script can be exact.
	for _, layout := range []string{"15:04", "15:04:05"} {
		if t, err := time.Parse(layout, spec); err == nil {
			at := time.Date(now.Year(), now.Month(), now.Day(),
				t.Hour(), t.Minute(), t.Second(), 0, now.Location())
			if !at.After(now) {
				at = at.AddDate(0, 0, 1)
			}
			return at, nil
		}
	}

	// A date, meaning midnight starting that day.
	for _, layout := range []string{"2006-01-02", "2006-01-02T15:04", "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, spec, now.Location()); err == nil {
			return t, nil
		}
	}

	return time.Time{}, fmt.Errorf("cannot read --until %q: expected a duration (2h), "+
		"a time (18:00), a date (2026-08-20), or one of reset, session, week", spec)
}

// Describe renders a lease as the phrase that goes after "in force".
func (l Lease) Describe(now time.Time) string {
	switch l.Kind {
	case LeaseManual:
		return "until revoked"
	case LeaseDeadline:
		if l.AtMS == nil {
			return "until an unset deadline"
		}
		at := time.UnixMilli(*l.AtMS)
		if d := time.Until(at); d > 0 {
			return fmt.Sprintf("until %s (%s)", at.Format("Mon 2 Jan 15:04"), roundDur(d))
		}
		return fmt.Sprintf("until %s (passed)", at.Format("Mon 2 Jan 15:04"))
	case LeaseWindowReset:
		if l.WindowEndMS == nil {
			return fmt.Sprintf("until the next %s window resets", BucketLabel(l.Bucket))
		}
		at := time.UnixMilli(*l.WindowEndMS)
		return fmt.Sprintf("until the %s window resets %s", BucketLabel(l.Bucket), at.Format("Mon 2 Jan 15:04"))
	default:
		return "until " + l.Kind
	}
}

func roundDur(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%.1fd", d.Hours()/24)
	case d >= time.Hour:
		return fmt.Sprintf("%.1fh", d.Hours())
	default:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
}
