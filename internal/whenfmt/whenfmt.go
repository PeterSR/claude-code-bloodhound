// Package whenfmt renders a moment in the future the way a person reads it.
//
// Extracted from the weaverbird widgets when the budget announcements needed
// the same rendering. A message that says a limit is about to bite has to say
// when it stops biting, and "2026-08-22T13:00:00Z" is not that answer for
// someone whose clock says Copenhagen. Deadline is where the judgment lives:
// under a day it counts down, past a day it names the weekday, and it knows
// about local midnights and daylight saving because both of those put a
// deadline on the wrong day otherwise.
//
// Two callers now, which is what moved this out of cmd/bloodhound. The
// arithmetic is small but it is not obvious, and the tests that pin it are
// worth having in one place.
package whenfmt

import (
	"fmt"
	"strings"
	"time"
)

// Tuning for Deadline. Consts rather than config fields: each is a
// judgment about what a human reads well, not about one machine's setup,
// and none of them is worth the plumbing of a config knob until somebody
// actually wants a different answer. Moving any of them into
// config.Config later is additive.
const (
	// absoluteResetAfter is where a countdown stops being the more useful
	// rendering. Under a day, "19h" is something you can act on directly.
	// Past it, "2d15h" makes you do calendar arithmetic to answer the
	// question you actually asked, which is what day you get quota back.
	absoluteResetAfter = 24 * time.Hour

	// midnightGrace is how far either side of local midnight a reset may
	// fall and still print as a bare day name. Nothing about a weekly
	// quota is precise enough for three hours to change a plan, and
	// "fri" is materially easier to read than "fri 23:00".
	//
	// The band is applied by rounding to the NEAREST midnight and naming
	// the day that midnight opens, not by naming the day the reset falls
	// on. That is what makes the two halves of the band equivalent: a
	// Thursday 22:00 reset and a Friday 02:00 reset both print "fri",
	// which is the whole point of having a grace band. It also fails in
	// the safe direction — the named day is never earlier than the real
	// reset, so the label never promises quota back before it exists.
	midnightGrace = 3 * time.Hour

	// maxNamedDayOffset is the largest number of calendar days ahead a
	// weekday name can unambiguously identify: today plus six covers
	// seven distinct names, and day seven repeats today's. A weekly
	// window can sit a full 7 days out, and rounding up to a midnight can
	// push it further, so without this bound a reset a week away would
	// print the current weekday and read as "already reset".
	maxNamedDayOffset = 6
)

// Deadline renders when a deadline falls, choosing between a countdown
// and an absolute local weekday by how far out it is:
//
//	19h          under absoluteResetAfter: a countdown answers it directly
//	fri          beyond that, within midnightGrace of a local midnight
//	fri 15:30    beyond that, anywhere else in the day
//	7d           too far out for a weekday name to be unique (see below)
//
// This is deliberately general rather than week-shaped, and the callers
// need no bucket-specific branching because of it: a 5-hour window is
// never a day away, so it can only ever take the first branch, while a
// weekly one usually takes the second or third. The rule is about the
// distance, not about which quota is being described.
//
// now supplies the zone as well as the instant. Reset timestamps arrive
// as ISO-8601 UTC and a weekday computed in UTC is simply wrong for the
// person reading it — a Friday 23:30 UTC reset is Saturday on a
// Copenhagen clock. Taking the zone from the caller's own clock rather
// than from time.Local keeps the function pure and testable.
//
// The maxNamedDayOffset fallback is the subtle one. A weekly window can
// legitimately sit almost 7 days out, at which point "fri" names the same
// weekday as today and reads as though the reset already happened. When
// the named day is too far ahead to be unique, the countdown is the only
// honest rendering left, so it goes back to it.
func Deadline(at, now time.Time) string {
	text, _ := deadline(at, now)
	return text
}

// Phrase renders a deadline as a fragment that reads inside a sentence:
// "in 42m" for a countdown, "on fri 15:30" for a named day. The preposition
// has to follow the branch Deadline took, which is why that branch is
// reported rather than guessed at from the shape of the string.
func Phrase(at, now time.Time) string {
	text, absolute := deadline(at, now)
	if absolute {
		return "on " + text
	}
	return "in " + text
}

// deadline reports the rendering and whether it names a moment rather than
// counting down to one.
func deadline(at, now time.Time) (text string, absolute bool) {
	d := at.Sub(now)
	countdown := Dur(d.Milliseconds())
	if d < absoluteResetAfter {
		return countdown, false
	}

	local := at.In(now.Location())

	// named is the day whose name gets printed, which is not always the
	// day the reset falls on: inside the grace band it is the day opened
	// by the nearest midnight, which for a late-evening reset is the day
	// after. bare tracks that case, since a rounded deadline has no
	// time of day left worth printing.
	named, bare := local, false
	if m, ok := nearestMidnight(local); ok {
		named, bare = m, true
	}

	if n := daysBetween(now.In(now.Location()), named); n < 1 || n > maxNamedDayOffset {
		return countdown, false
	}

	name := strings.ToLower(named.Format("Mon"))
	if bare {
		return name, true
	}
	return name + " " + local.Format("15:04"), true
}

// nearestMidnight returns the local midnight closest to t and whether t is
// within midnightGrace of it. The two candidates are the midnight that
// opened t's own day and the one that opens the next; ties (t exactly at
// noon) go to the earlier, which cannot matter since noon is never inside
// any sane grace band.
func nearestMidnight(t time.Time) (time.Time, bool) {
	start := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	next := start.AddDate(0, 0, 1)
	if t.Sub(start) <= next.Sub(t) {
		return start, t.Sub(start) <= midnightGrace
	}
	return next, next.Sub(t) <= midnightGrace
}

// daysBetween counts whole calendar days from's date to to's date in
// from's zone, which is not the same as dividing their difference by 24
// hours: across a DST boundary a calendar day is 23 or 25 hours long, and
// truncating that would put a deadline one day off twice a year. Both
// ends are floored to midnight first so only the dates matter, and the
// rounding then absorbs the hour a DST shift adds or removes.
func daysBetween(from, to time.Time) int {
	f := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, from.Location())
	t := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, from.Location())
	return int(t.Sub(f).Hours()/24 + 0.5)
}

// Dur renders a millisecond duration coarse-to-fine ("33m", "3h12m",
// "5d3h").
//
// internal/statusline still has its own fmtDur, deliberately: it renders a
// time.Duration rather than milliseconds, it predates this package, and its
// output is pinned by that package's own tests. It is a one-liner and leaving
// it alone costs less than churning a shipped surface. Deadline is the reason
// this package exists; Dur came along because Deadline falls back to it.
func Dur(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	}
	day := int(d.Hours()) / 24
	rh := int(d.Hours()) - day*24
	if rh == 0 {
		return fmt.Sprintf("%dd", day)
	}
	return fmt.Sprintf("%dd%dh", day, rh)
}
