package store

import (
	"testing"
	"time"
)

// seq builds a bucket series from plain percentages, 5 minutes apart, with
// no reset-time hints. A negative value means "no reading" (parse gap).
func seq(pcts ...int) []pctReading {
	out := make([]pctReading, len(pcts))
	for i, p := range pcts {
		out[i] = pctReading{TSUnixMS: int64(i) * 5 * 60 * 1000}
		if p >= 0 {
			out[i].Pct, out[i].HasPct = p, true
		}
	}
	return out
}

func resets(flags []pctFlags) []int {
	var out []int
	for i, f := range flags {
		if f.ResetDetected {
			out = append(out, i)
		}
	}
	return out
}

func invalids(flags []pctFlags) []int {
	var out []int
	for i, f := range flags {
		if !f.Valid {
			out = append(out, i)
		}
	}
	return out
}

func eqInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestClassifyDetectsTheMissedSmallReset(t *testing.T) {
	// The exact bug: session pct read 18 then dropped to 0 and stayed. An
	// 18-point drop never tripped the old 30-point threshold, so the reset
	// went unflagged. Under the peak rule it is unambiguous.
	f := classifyBucket(seq(15, 17, 18, 0, 0, 1, 2))
	if got := resets(f); !eqInts(got, []int{3}) {
		t.Errorf("reset at index %v, want [3]", got)
	}
	if got := invalids(f); len(got) != 0 {
		t.Errorf("no misparse expected, got invalids at %v", got)
	}
}

func TestClassifyTreatsRoundingJitterAsNeitherResetNorMisparse(t *testing.T) {
	// 18, 17, 17, 18: a flat true value rounding across a boundary. A
	// 1-point drop is the most rounding can do, so nothing here is a reset
	// or a bad reading.
	f := classifyBucket(seq(18, 17, 17, 18, 25))
	if got := resets(f); len(got) != 0 {
		t.Errorf("no reset expected, got %v", got)
	}
	if got := invalids(f); len(got) != 0 {
		t.Errorf("no misparse expected, got %v", got)
	}
}

func TestClassifyMarksRecoveringDipAsMisparse(t *testing.T) {
	// The other real bug: week pct read 34, 34, 34, 16, 34, 34. A weekly
	// total cannot fall 18 points and recover 5 minutes later, so the 16 is
	// a misparse — invalid, but not a reset.
	f := classifyBucket(seq(34, 34, 34, 16, 34, 34))
	if got := invalids(f); !eqInts(got, []int{3}) {
		t.Errorf("misparse at index %v, want [3]", got)
	}
	if got := resets(f); len(got) != 0 {
		t.Errorf("a misparse is not a reset, got resets at %v", got)
	}
}

func TestClassifyDistinguishesPersistentDropFromRecovery(t *testing.T) {
	// Same shape of drop, opposite meaning, decided only by what follows.
	recover := classifyBucket(seq(40, 40, 12, 40, 41))
	if !eqInts(invalids(recover), []int{2}) || len(resets(recover)) != 0 {
		t.Errorf("recovering dip: invalids=%v resets=%v, want misparse at 2",
			invalids(recover), resets(recover))
	}
	persist := classifyBucket(seq(40, 40, 12, 13, 15))
	if !eqInts(resets(persist), []int{2}) || len(invalids(persist)) != 0 {
		t.Errorf("persistent drop: resets=%v invalids=%v, want reset at 2",
			resets(persist), invalids(persist))
	}
}

func TestClassifyBigCleanReset(t *testing.T) {
	// The ordinary case the old threshold did catch: near-cap then rollover.
	f := classifyBucket(seq(88, 95, 97, 2, 6, 9))
	if got := resets(f); !eqInts(got, []int{3}) {
		t.Errorf("reset at %v, want [3]", got)
	}
}

func TestClassifyHandlesParseGaps(t *testing.T) {
	// A missing reading (bucket didn't parse that poll) is a hole, not a
	// reset and not a misparse. The window continues around it.
	f := classifyBucket(seq(10, 12, -1, 14, 16))
	if len(resets(f)) != 0 || len(invalids(f)) != 0 {
		t.Errorf("parse gap disturbed classification: resets=%v invalids=%v",
			resets(f), invalids(f))
	}
}

func TestClassifyBoundaryHeuristicCatchesQuietRotation(t *testing.T) {
	// Idle across a reset: the percentage barely moved, but the clock
	// passed the reset time a prior reading advertised. Still a reset.
	base := int64(1_000_000_000_000)
	resetAt := time.UnixMilli(base).Add(30 * time.Minute)
	r := []pctReading{
		{TSUnixMS: base, Pct: 40, HasPct: true, ResetTS: resetAt, HasReset: true},
		{TSUnixMS: base + int64(60*60*1000), Pct: 44, HasPct: true}, // an hour later, past the reset
		{TSUnixMS: base + int64(90*60*1000), Pct: 46, HasPct: true},
	}
	f := classifyBucket(r)
	if got := resets(f); !eqInts(got, []int{1}) {
		t.Errorf("boundary reset at %v, want [1]", got)
	}
}

func TestClassifyEmptyAndSingle(t *testing.T) {
	if len(classifyBucket(nil)) != 0 {
		t.Error("nil series should classify to nothing")
	}
	f := classifyBucket(seq(50))
	if len(f) != 1 || f[0].ResetDetected || !f[0].Valid {
		t.Errorf("single reading misclassified: %+v", f)
	}
}
