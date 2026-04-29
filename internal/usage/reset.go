package usage

import (
	"regexp"
	"strings"
	"time"
)

// The TUI emits reset hints with no whitespace ("May1,1am"). Normalize to a
// canonical form ("May 1, 1am") before handing off to time.Parse.
var (
	letterDigitRe = regexp.MustCompile(`([A-Za-z])(\d)`)
	resetSpacesRe = regexp.MustCompile(`\s+`)
)

func normalizeReset(s string) string {
	s = letterDigitRe.ReplaceAllString(s, "$1 $2")
	s = strings.ReplaceAll(s, ",", ", ")
	s = resetSpacesRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// ParseReset converts a /usage "Resets …" hint into an absolute UTC time.
//
// The TUI emits the wall-clock time ("10:50am") and the IANA timezone
// ("Europe/Copenhagen") as separate fields; pass both. tz is required for
// correctness — wall-clock strings without a zone are ambiguous, and
// silently treating "10:50am" as UTC produces times that drift by hours
// from the user's actual reset window. If tz is unknown / unloadable the
// function falls back to now.Location() and the result is best-effort.
//
// Inputs we've observed in the wild:
//
//	"May 1, 1am"        + "Europe/Copenhagen"
//	"May 1, 12am"       + "Europe/Copenhagen"
//	"10:50am"           + "Europe/Copenhagen"
//	"1am"               + "America/Los_Angeles"
//	"15:04"             + ""  (timezone not captured; fallback)
//
// Returns (UTC time, true) on success, (zero, false) on parse failure.
func ParseReset(s, tz string, now time.Time) (time.Time, bool) {
	s = normalizeReset(s)
	if s == "" {
		return time.Time{}, false
	}
	loc := now.Location()
	if tz = strings.TrimSpace(tz); tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	nowInLoc := now.In(loc)

	// Layouts that include a date.
	dateLayouts := []string{
		"Jan 2, 3:04pm",
		"Jan 2, 3pm",
		"Jan 2, 15:04",
	}
	for _, layout := range dateLayouts {
		t, err := time.Parse(layout, s)
		if err != nil {
			continue
		}
		out := time.Date(nowInLoc.Year(), t.Month(), t.Day(),
			t.Hour(), t.Minute(), 0, 0, loc)
		if out.Before(now) {
			out = out.AddDate(1, 0, 0)
		}
		return out.UTC(), true
	}

	// Time-only layouts: nearest future occurrence in the target TZ.
	timeLayouts := []string{
		"3:04pm",
		"3pm",
		"15:04",
	}
	for _, layout := range timeLayouts {
		t, err := time.Parse(layout, s)
		if err != nil {
			continue
		}
		out := time.Date(nowInLoc.Year(), nowInLoc.Month(), nowInLoc.Day(),
			t.Hour(), t.Minute(), 0, 0, loc)
		if out.Before(now) {
			out = out.AddDate(0, 0, 1)
		}
		return out.UTC(), true
	}

	return time.Time{}, false
}
