package nowstate

import (
	"context"
	"testing"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

// newTestStore opens a throwaway, migrated store under a per-test
// XDG_DATA_HOME. t.Setenv scopes the env var to this test (and restores it
// afterward), so this is safe to run alongside anything else touching the
// real environment.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	s, err := store.Open(context.Background())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// insertObs writes one usage_observations row. resetISO == "" leaves the
// column NULL (unparsed reset); the other rows in a series only need
// sessionPct + resetDetected — buildWindow reads reset_ts off the LATEST
// row only, so only that row's resetISO actually matters for the window
// shape, while every row's sessionPct feeds the burn-rate query.
func insertObs(t *testing.T, s *store.Store, tsMS int64, sessionPct int, resetISO string, resetDetected bool) {
	t.Helper()
	var resetISOArg any
	if resetISO != "" {
		resetISOArg = resetISO
	}
	rd := 0
	if resetDetected {
		rd = 1
	}
	_, err := s.DB.Exec(`
		INSERT INTO usage_observations (
			ts, ts_unix_ms, session_pct, week_pct,
			session_reset_ts, session_reset_detected, week_reset_detected,
			parse_ok
		) VALUES (?, ?, ?, NULL, ?, ?, 0, 1)`,
		time.UnixMilli(tsMS).UTC().Format(time.RFC3339), tsMS, sessionPct, resetISOArg, rd,
	)
	if err != nil {
		t.Fatalf("insert obs at %d: %v", tsMS, err)
	}
}

func TestCompute_NoObservationYieldsEmptyPoolState(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()

	out, err := Compute(context.Background(), s, now)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if out.LastPoll != nil {
		t.Errorf("LastPoll = %+v, want nil (no observation yet)", out.LastPoll)
	}
	if out.Session != nil || out.Week != nil {
		t.Errorf("Session/Week = %+v / %+v, want both nil", out.Session, out.Week)
	}
	if out.OK {
		t.Error("OK = true, want false")
	}
	if out.NowMS != now.UnixMilli() {
		t.Errorf("NowMS = %d, want %d", out.NowMS, now.UnixMilli())
	}
}

func TestCompute_BuildsWindowAndBurnFromHistory(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()

	// A window that started 31 minutes ago (session_reset_detected=1) and
	// has climbed 10 -> 12 -> 14 since, with a natural reset 2 hours out.
	// Every poll before the latest one needs no reset_ts: buildWindow only
	// reads it off the row LatestUsage returns.
	t0 := now.Add(-31 * time.Minute).UnixMilli()
	t1 := now.Add(-16 * time.Minute).UnixMilli()
	t2 := now.Add(-1 * time.Minute).UnixMilli()
	resetISO := now.Add(2 * time.Hour).UTC().Format(time.RFC3339)

	insertObs(t, s, t0, 10, "", true)
	insertObs(t, s, t1, 12, "", false)
	insertObs(t, s, t2, 14, resetISO, false)

	out, err := Compute(context.Background(), s, now)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}

	if out.LastPoll == nil {
		t.Fatal("LastPoll = nil, want the latest observation")
	}
	if out.LastPoll.AgeS < 55 || out.LastPoll.AgeS > 65 {
		t.Errorf("LastPoll.AgeS = %d, want ~60 (latest poll was 1 minute ago)", out.LastPoll.AgeS)
	}

	if out.Session == nil {
		t.Fatal("Session = nil, want a populated window")
	}
	if out.Session.Pct != 14 {
		t.Errorf("Session.Pct = %d, want 14 (the latest reading)", out.Session.Pct)
	}
	if out.Session.TimeToResetMS <= 0 {
		t.Errorf("Session.TimeToResetMS = %d, want > 0 (reset is 2h out)", out.Session.TimeToResetMS)
	}
	if !out.Session.BurnOK {
		t.Fatal("Session.BurnOK = false, want true (three points, 30 min of usable history)")
	}
	// 10 -> 14 over the 30 minutes between t0 and t2 = 8%/hour.
	if got, want := out.Session.BurnPctPerHour, 8.0; got < want-0.1 || got > want+0.1 {
		t.Errorf("Session.BurnPctPerHour = %v, want ~%v", got, want)
	}
	// Projected time to 100% (86 points at 8%/hour ≈ 10.75h) is well past
	// the 2h natural reset, so the limit projection must be suppressed.
	if out.Session.LimitOK {
		t.Error("Session.LimitOK = true, want false (projected limit is after the natural reset)")
	}

	if out.Week != nil {
		t.Errorf("Week = %+v, want nil (no week_pct was ever recorded)", out.Week)
	}
}

func TestCompute_OmitsBurnRateWithOnlyOneObservation(t *testing.T) {
	// The real-world case the ask calls out: a single poll (e.g. right
	// after `bloodhound poll` on a fresh install) has nowhere to measure a
	// slope from. The burn rate must be absent, not a reported 0.
	s := newTestStore(t)
	now := time.Now()
	insertObs(t, s, now.Add(-30*time.Second).UnixMilli(), 3, "", true)

	out, err := Compute(context.Background(), s, now)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if out.Session == nil {
		t.Fatal("Session = nil, want a window (session_pct was recorded)")
	}
	if out.Session.BurnOK {
		t.Error("Session.BurnOK = true, want false (only one observation, no slope to measure)")
	}
	if out.Session.BurnPctPerHour != 0 {
		t.Errorf("Session.BurnPctPerHour = %v, want the unset zero value", out.Session.BurnPctPerHour)
	}
}

func TestBuildWindow_UnparsedResetLeavesOnlyPctAndDetectedFlag(t *testing.T) {
	ws := buildWindow(55, "", 5*time.Hour, true, time.Now())
	if ws.Pct != 55 || !ws.ResetDetected {
		t.Errorf("ws = %+v, want Pct=55, ResetDetected=true", ws)
	}
	if ws.ResetTSISO != "" || ws.WindowStartTSISO != "" || ws.TimeToResetMS != 0 {
		t.Errorf("ws = %+v, want reset/window/TTR all zero (no reset string to parse)", ws)
	}
}

func TestBuildWindow_MalformedResetIsTreatedTheSameAsMissing(t *testing.T) {
	ws := buildWindow(10, "not-a-timestamp", 5*time.Hour, false, time.Now())
	if ws.ResetTSISO != "" {
		t.Errorf("ResetTSISO = %q, want empty on a parse failure", ws.ResetTSISO)
	}
}

func TestBuildWindow_PastResetLeavesTimeToResetAtZero(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Hour).UTC().Format(time.RFC3339)
	ws := buildWindow(90, past, 5*time.Hour, true, now)
	if ws.TimeToResetMS != 0 {
		t.Errorf("TimeToResetMS = %d, want 0 (reset already passed — a stale reading, not a negative countdown)", ws.TimeToResetMS)
	}
}

func TestBuildWindow_FutureResetComputesWindowStartAndTTR(t *testing.T) {
	now := time.Now()
	future := now.Add(3 * time.Hour).UTC().Format(time.RFC3339)
	ws := buildWindow(20, future, 5*time.Hour, false, now)

	if ws.ResetTSISO == "" {
		t.Fatal("ResetTSISO empty, want the parsed reset")
	}
	wantTTRms := 3 * time.Hour.Milliseconds()
	if diff := ws.TimeToResetMS - wantTTRms; diff < -1000 || diff > 1000 {
		t.Errorf("TimeToResetMS = %d, want ~%d", ws.TimeToResetMS, wantTTRms)
	}

	start, err := time.Parse(time.RFC3339, ws.WindowStartTSISO)
	if err != nil {
		t.Fatalf("WindowStartTSISO %q didn't parse: %v", ws.WindowStartTSISO, err)
	}
	resetT, _ := time.Parse(time.RFC3339, ws.ResetTSISO)
	if got := resetT.Sub(start); got != 5*time.Hour {
		t.Errorf("reset - windowStart = %v, want the 5h span", got)
	}
}
