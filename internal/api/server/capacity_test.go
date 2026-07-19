package server

import (
	"testing"
	"time"
)

// at builds a unix-ms timestamp for a UTC wall-clock moment.
func at(y, mo, d, h, mi int) int64 {
	return time.Date(y, time.Month(mo), d, h, mi, 0, 0, time.UTC).UnixMilli()
}

// rfc formats a UTC moment the way session_reset_ts / week_reset_ts are stored.
func rfc(y, mo, d, h, mi int) string {
	return time.Date(y, time.Month(mo), d, h, mi, 0, 0, time.UTC).Format(time.RFC3339)
}

// cob builds a capacity observation carrying only the week-side fields the
// reconstruction reads.
func cob(tsMS int64, weekPct int, valid, saturated bool, sessReset, weekReset string) capObs {
	v := weekPct
	return capObs{
		tsMS:        tsMS,
		weekPct:     &v,
		weekValid:   valid,
		weekSat:     saturated,
		sessResetTS: sessReset,
		weekResetTS: weekReset,
	}
}

func TestWeeklyCapacity_Empty(t *testing.T) {
	out := weeklyCapacity(nil)
	if !out.OK {
		t.Fatalf("OK = false")
	}
	if len(out.Weeks) != 0 {
		t.Fatalf("Weeks = %d, want 0", len(out.Weeks))
	}
	if out.SessionCount != 0 || out.SessionsPerWeek != 0 {
		t.Fatalf("summary should be empty: count=%d perWeek=%v", out.SessionCount, out.SessionsPerWeek)
	}
}

func TestWeeklyCapacity_TypicalSessions(t *testing.T) {
	// One weekly window (reset Jun 11 23:00). Five 5h sessions keyed by
	// distinct session resets: a leading partial, three complete +10% work
	// sessions, and a trailing in-progress one; only the three middle ones
	// feed the median.
	wr := rfc(2026, 6, 11, 23, 0)
	sA := rfc(2026, 6, 8, 5, 0)
	sB := rfc(2026, 6, 8, 10, 0)
	sC := rfc(2026, 6, 8, 15, 0)
	sD := rfc(2026, 6, 8, 20, 0)
	sE := rfc(2026, 6, 9, 1, 0)
	obs := []capObs{
		cob(at(2026, 6, 8, 0, 0), 0, true, false, sA, wr), // session A (partial)
		cob(at(2026, 6, 8, 2, 0), 0, true, false, sA, wr),
		cob(at(2026, 6, 8, 5, 0), 0, true, false, sB, wr), // session B: 0 -> 10
		cob(at(2026, 6, 8, 7, 0), 10, true, false, sB, wr),
		cob(at(2026, 6, 8, 10, 0), 10, true, false, sC, wr), // session C: 10 -> 20
		cob(at(2026, 6, 8, 12, 0), 20, true, false, sC, wr),
		cob(at(2026, 6, 8, 15, 0), 20, true, false, sD, wr), // session D: 20 -> 30
		cob(at(2026, 6, 8, 17, 0), 30, true, false, sD, wr),
		cob(at(2026, 6, 8, 20, 0), 30, true, false, sE, wr), // session E (in progress)
		cob(at(2026, 6, 8, 22, 0), 40, true, false, sE, wr),
	}
	out := weeklyCapacity(obs)

	if out.SessionCount != 3 {
		t.Errorf("SessionCount = %d, want 3", out.SessionCount)
	}
	if out.TypicalSessionWeekPct != 10 {
		t.Errorf("TypicalSessionWeekPct = %v, want 10", out.TypicalSessionWeekPct)
	}
	if out.SessionsPerWeek != 10 {
		t.Errorf("SessionsPerWeek = %v, want 10", out.SessionsPerWeek)
	}
	if out.DaysPerSession != 0.7 {
		t.Errorf("DaysPerSession = %v, want 0.7", out.DaysPerSession)
	}
	if len(out.Weeks) != 1 {
		t.Fatalf("Weeks = %d, want 1", len(out.Weeks))
	}
	wk := out.Weeks[0]
	if !wk.InProgress {
		t.Errorf("only week should be in progress")
	}
	if wk.TotalWeekPct != 40 {
		t.Errorf("week TotalWeekPct = %v, want 40 (net rise 0..40)", wk.TotalWeekPct)
	}
	if len(wk.Sessions) != 4 { // B, C, D complete + the in-progress session's slice
		t.Errorf("week Sessions = %d, want 4", len(wk.Sessions))
	}
}

func TestWeeklyCapacity_WeekResetSplits(t *testing.T) {
	// A single 5h session spans a weekly reset: two weeks, one session.
	s1 := rfc(2026, 6, 11, 6, 0)
	wr1 := rfc(2026, 6, 11, 23, 0)
	wr2 := rfc(2026, 6, 18, 23, 0)
	obs := []capObs{
		cob(at(2026, 6, 11, 0, 0), 10, true, false, s1, wr1),
		cob(at(2026, 6, 11, 2, 0), 20, true, false, s1, wr1), // week 1: 10 -> 20
		cob(at(2026, 6, 11, 3, 0), 0, true, false, s1, wr2),  // weekly reset
		cob(at(2026, 6, 11, 5, 0), 10, true, false, s1, wr2), // week 2: 0 -> 10
	}
	out := weeklyCapacity(obs)

	if len(out.Weeks) != 2 {
		t.Fatalf("Weeks = %d, want 2", len(out.Weeks))
	}
	if out.Weeks[0].InProgress {
		t.Errorf("first week should be closed")
	}
	if out.Weeks[0].ResetUnixMS == 0 {
		t.Errorf("first week should record a reset boundary")
	}
	if out.Weeks[0].TotalWeekPct != 10 {
		t.Errorf("first week total = %v, want 10", out.Weeks[0].TotalWeekPct)
	}
	if !out.Weeks[1].InProgress || out.Weeks[1].TotalWeekPct != 10 {
		t.Errorf("second week: inProgress=%v total=%v, want true/10", out.Weeks[1].InProgress, out.Weeks[1].TotalWeekPct)
	}
	if out.SessionCount != 0 || out.SessionsPerWeek != 0 {
		t.Errorf("no session-reset boundaries means no rate: count=%d perWeek=%v", out.SessionCount, out.SessionsPerWeek)
	}
}

func TestWeeklyCapacity_NetRiseIgnoresSaturatedAndMisparse(t *testing.T) {
	// One complete session whose net rise is 0 -> 10; a saturated spike and a
	// misparsed dip in the middle must not distort the net.
	wr := rfc(2026, 6, 11, 23, 0)
	sA := rfc(2026, 6, 8, 5, 0)
	sB := rfc(2026, 6, 8, 10, 0)
	sC := rfc(2026, 6, 8, 15, 0)
	obs := []capObs{
		cob(at(2026, 6, 8, 0, 0), 0, true, false, sA, wr),   // session A (partial)
		cob(at(2026, 6, 8, 5, 0), 0, true, false, sB, wr),   // session B first valid = 0
		cob(at(2026, 6, 8, 6, 0), 50, true, true, sB, wr),   // saturated -> ignored
		cob(at(2026, 6, 8, 7, 0), 5, false, false, sB, wr),  // misparse -> ignored
		cob(at(2026, 6, 8, 8, 0), 10, true, false, sB, wr),  // session B last valid = 10
		cob(at(2026, 6, 8, 10, 0), 10, true, false, sC, wr), // session C (in progress)
	}
	out := weeklyCapacity(obs)

	if out.SessionCount != 1 {
		t.Errorf("SessionCount = %d, want 1", out.SessionCount)
	}
	if len(out.Weeks) != 1 {
		t.Fatalf("Weeks = %d, want 1", len(out.Weeks))
	}
	if out.Weeks[0].TotalWeekPct != 10 {
		t.Errorf("week total = %v, want 10 (net rise, noise excluded)", out.Weeks[0].TotalWeekPct)
	}
	if !out.Weeks[0].HitCap {
		t.Errorf("week should be flagged HitCap (a saturated reading occurred)")
	}
}

func TestWeeklyCapacity_RunningPeakIgnoresDipAtBoundary(t *testing.T) {
	// week_pct dips (an unflagged misparse) right at a session boundary and
	// recovers. Summing per-slice net rises would double-count the recovery
	// (30 + 20 = 50); the running weekly peak counts only the real rise (30).
	wr := rfc(2026, 6, 11, 23, 0)
	sA := rfc(2026, 6, 8, 5, 0)
	sB := rfc(2026, 6, 8, 10, 0)
	sC := rfc(2026, 6, 8, 15, 0)
	obs := []capObs{
		cob(at(2026, 6, 8, 0, 0), 0, true, false, sA, wr),   // session A (partial)
		cob(at(2026, 6, 8, 5, 0), 0, true, false, sB, wr),   // session B: 0 -> 30
		cob(at(2026, 6, 8, 7, 0), 30, true, false, sB, wr),  // peak 30
		cob(at(2026, 6, 8, 10, 0), 10, true, false, sC, wr), // session C opens on a dip
		cob(at(2026, 6, 8, 12, 0), 30, true, false, sC, wr), // recovers to 30, no new peak
	}
	out := weeklyCapacity(obs)

	if len(out.Weeks) != 1 {
		t.Fatalf("Weeks = %d, want 1", len(out.Weeks))
	}
	if out.Weeks[0].TotalWeekPct != 30 {
		t.Errorf("week total = %v, want 30 (dip recovery not double-counted)", out.Weeks[0].TotalWeekPct)
	}
	if out.SessionCount != 1 {
		t.Errorf("SessionCount = %d, want 1", out.SessionCount)
	}
}

func TestWeeklyCapacity_WorkSessionFloorExcludesCheckins(t *testing.T) {
	// Real, bounded sessions that each lift the week only 2% never publish a
	// rate: they sit below the 3% work-session floor even though several are
	// complete.
	wr := rfc(2026, 6, 11, 23, 0)
	var obs []capObs
	for i := 0; i < 6; i++ {
		start := i * 5                    // 5h session windows, resets 5h apart
		sr := rfc(2026, 6, 8, start+5, 0) // this window's advertised reset
		obs = append(obs, cob(at(2026, 6, 8, start, 0), i*2, true, false, sr, wr))
		obs = append(obs, cob(at(2026, 6, 8, start+2, 0), i*2+2, true, false, sr, wr))
	}
	out := weeklyCapacity(obs)
	if out.SessionCount != 0 {
		t.Errorf("SessionCount = %d, want 0 (each session below the 3%% floor)", out.SessionCount)
	}
	if out.SessionsPerWeek != 0 {
		t.Errorf("SessionsPerWeek = %v, want 0", out.SessionsPerWeek)
	}
}

func TestWeeklyCapacity_MisparsedResetDoesNotSplit(t *testing.T) {
	// One poll misreads the weekly reset by a few hours, far short of a real
	// 7-day rotation. It must not fabricate a second weekly window: the true
	// peak of 50 belongs to a single week.
	wr := rfc(2026, 6, 11, 23, 0)
	bad := rfc(2026, 6, 12, 3, 0) // wr + 4h: a misparse, not a rotation
	s1 := rfc(2026, 6, 8, 6, 0)
	obs := []capObs{
		cob(at(2026, 6, 8, 0, 0), 0, true, false, s1, wr),
		cob(at(2026, 6, 8, 1, 0), 40, true, false, s1, wr),
		cob(at(2026, 6, 8, 2, 0), 40, true, false, s1, bad), // misparsed reset row
		cob(at(2026, 6, 8, 3, 0), 45, true, false, s1, wr),
		cob(at(2026, 6, 8, 4, 0), 50, true, false, s1, wr),
	}
	out := weeklyCapacity(obs)
	if len(out.Weeks) != 1 {
		t.Fatalf("Weeks = %d, want 1 (misparsed reset must not split)", len(out.Weeks))
	}
	if out.Weeks[0].TotalWeekPct != 50 {
		t.Errorf("week total = %v, want 50", out.Weeks[0].TotalWeekPct)
	}
}

func TestWeeklyCapacity_IgnoresImplausibleReset(t *testing.T) {
	// A single row with a year-typo week reset must not spawn a bogus week.
	wr := rfc(2026, 6, 11, 23, 0)
	bad := rfc(2027, 6, 11, 23, 0) // ~a year ahead: implausible
	s1 := rfc(2026, 6, 8, 6, 0)
	obs := []capObs{
		cob(at(2026, 6, 8, 0, 0), 10, true, false, s1, wr),
		cob(at(2026, 6, 8, 1, 0), 11, true, false, s1, bad), // carried forward
		cob(at(2026, 6, 8, 2, 0), 12, true, false, s1, wr),
	}
	out := weeklyCapacity(obs)
	if len(out.Weeks) != 1 {
		t.Fatalf("Weeks = %d, want 1 (implausible reset ignored)", len(out.Weeks))
	}
}
