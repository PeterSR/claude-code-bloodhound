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

// ParseReset best-effort converts a /usage "Resets …" hint into an absolute
// time in the local zone of `now`. Inputs we've observed include:
//
//	"May 1, 1am"
//	"May 1, 12am"
//	"10pm"
//	"1am"
//	"15:04"
//
// Returns ok=false on any parse failure. Callers should persist the raw
// string regardless.
func ParseReset(s string, now time.Time) (time.Time, bool) {
	s = normalizeReset(s)
	if s == "" {
		return time.Time{}, false
	}
	loc := now.Location()
	year := now.Year()

	// Layouts that include a date component.
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
		out := time.Date(year, t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, loc)
		if out.Before(now) {
			out = out.AddDate(1, 0, 0)
		}
		return out, true
	}

	// Time-only layouts: assume nearest future occurrence.
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
		out := time.Date(year, now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, loc)
		if out.Before(now) {
			out = out.AddDate(0, 0, 1)
		}
		return out, true
	}

	return time.Time{}, false
}
