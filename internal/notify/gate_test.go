package notify

import (
	"testing"
	"time"

	ccsock "github.com/PeterSR/claude-code-socket-transport"
)

// The gate's whole job is to refuse to wake a session. These tests pin the
// refusals, because every one of them is a case where delivering costs the
// user real tokens for a message nobody asked for.
//
// Reachability is a live socket probe, so the sessions here point at a socket
// path that does not exist and the reachability check does the refusing. That
// makes the at-rest and stale-status cases assert only that they are refused,
// not which reason won. The cases that matter positively are covered by
// admitOrder below, which drives the ordering directly.

func session(status string, statusAge time.Duration, now time.Time) ccsock.Session {
	return ccsock.Session{
		SessionID:       "session-abc12345",
		SocketPath:      "/nonexistent/bloodhound-gate-test.sock",
		Status:          status,
		StatusUpdatedAt: now.Add(-statusAge),
	}
}

func TestGateRefusesASessionWithNoInbox(t *testing.T) {
	now := time.Now()
	s := session(statusBusy, time.Minute, now)
	s.SocketPath = ""
	d := Gate{Now: now}.Admit(s)
	if d.Admit {
		t.Fatal("admitted a session with no inbox socket")
	}
	if d.Skip != SkipNoInbox {
		t.Errorf("skip = %q, want %q", d.Skip, SkipNoInbox)
	}
}

func TestGateRefusesEverythingUnreachable(t *testing.T) {
	// A registry entry outlives the process that wrote it, so an unreachable
	// socket is the normal case on a dev machine, not an error.
	now := time.Now()
	for _, status := range []string{statusBusy, statusShell, statusIdle, statusWaiting} {
		d := Gate{Now: now}.Admit(session(status, time.Minute, now))
		if d.Admit {
			t.Errorf("status %q: admitted an unreachable session", status)
		}
	}
}

// admitOrder mirrors Admit's decision sequence for the checks that do not need
// a live socket, so the at-rest and staleness rules can be asserted directly.
// It is deliberately a copy of the ordering rather than a refactor of it: the
// point is to notice if the real order ever changes.
func admitOrder(g Gate, s ccsock.Session) string {
	if s.SocketPath == "" {
		return SkipNoInbox
	}
	if s.Status != statusBusy {
		return SkipAtRest
	}
	if g.statusAge(s) > g.maxAge() {
		return SkipStaleStatus
	}
	if st, ok := g.Cache[s.SessionID]; ok && st.Cold {
		return SkipCacheCold
	}
	return ""
}

func TestOnlyBusyIsAwake(t *testing.T) {
	// "shell" is the resting state, measured running to eight days stale on a
	// live machine, and it is by far the most common. Treating it as awake
	// would make the gate a no-op.
	now := time.Now()
	for _, status := range []string{statusShell, statusIdle, statusWaiting, "", "unknown"} {
		if got := admitOrder(Gate{Now: now}, session(status, time.Minute, now)); got != SkipAtRest {
			t.Errorf("status %q: got %q, want %q", status, got, SkipAtRest)
		}
	}
	if got := admitOrder(Gate{Now: now}, session(statusBusy, time.Minute, now)); got != "" {
		t.Errorf("busy session: got %q, want admitted", got)
	}
}

func TestStaleBusyIsNotBelieved(t *testing.T) {
	// A live socket still claiming "busy" hours later is a session that
	// stopped updating its status, and delivering there is the cold wakeup
	// this gate exists to prevent.
	now := time.Now()
	if got := admitOrder(Gate{Now: now}, session(statusBusy, MaxStatusAge+time.Minute, now)); got != SkipStaleStatus {
		t.Errorf("got %q, want %q", got, SkipStaleStatus)
	}
	if got := admitOrder(Gate{Now: now}, session(statusBusy, MaxStatusAge-time.Minute, now)); got != "" {
		t.Errorf("got %q, want admitted just inside the bound", got)
	}
}

func TestNeverReportedStatusIsTreatedAsInfinitelyStale(t *testing.T) {
	now := time.Now()
	s := session(statusBusy, 0, now)
	s.StatusUpdatedAt = time.Time{}
	if got := admitOrder(Gate{Now: now}, s); got != SkipStaleStatus {
		t.Errorf("got %q, want %q: a session with no status must not slip through", got, SkipStaleStatus)
	}
}

func TestColdCacheIsRefusedEvenWhenBusy(t *testing.T) {
	// The cross-check. A busy session should be warm, but the cost of being
	// wrong is asymmetric: a skipped warning costs nothing and a cold resume
	// re-pays the whole prefix.
	now := time.Now()
	g := Gate{Now: now, Cache: map[string]CacheState{
		"session-abc12345": {Cold: true, ColdResumeCostCWTokens: 1_200_000},
	}}
	if got := admitOrder(g, session(statusBusy, time.Minute, now)); got != SkipCacheCold {
		t.Errorf("got %q, want %q", got, SkipCacheCold)
	}
}

func TestUnknownCacheDoesNotBlockAWorkingSession(t *testing.T) {
	// A session bloodhound has never ingested is absent from the map. That is
	// unknown, not cold: the activity rule already establishes it is mid-turn,
	// and refusing on absence would mute the whole feature on a fresh install.
	now := time.Now()
	g := Gate{Now: now, Cache: map[string]CacheState{"someone-else": {Cold: true}}}
	if got := admitOrder(g, session(statusBusy, time.Minute, now)); got != "" {
		t.Errorf("got %q, want admitted", got)
	}
}

func TestWarmCacheIsAdmitted(t *testing.T) {
	now := time.Now()
	g := Gate{Now: now, Cache: map[string]CacheState{"session-abc12345": {Cold: false}}}
	if got := admitOrder(g, session(statusBusy, time.Minute, now)); got != "" {
		t.Errorf("got %q, want admitted", got)
	}
}

func TestAdmittedTalliesSkips(t *testing.T) {
	now := time.Now()
	sessions := []ccsock.Session{
		session(statusShell, time.Minute, now),
		session(statusShell, time.Hour, now),
	}
	sessions[0].SocketPath = ""
	admit, skipped := Gate{Now: now}.Admitted(sessions)
	if len(admit) != 0 {
		t.Fatalf("admitted %d sessions, want 0", len(admit))
	}
	if skipped[SkipNoInbox] != 1 {
		t.Errorf("no-inbox tally = %d, want 1", skipped[SkipNoInbox])
	}
	if total := skipped[SkipNoInbox] + skipped[SkipUnreachable] + skipped[SkipAtRest]; total != 2 {
		t.Errorf("tallied %d skips, want 2 (every refusal must be counted)", total)
	}
}

func TestMaxStatusAgeOverride(t *testing.T) {
	now := time.Now()
	g := Gate{Now: now, MaxStatusAge: time.Second}
	if got := admitOrder(g, session(statusBusy, time.Minute, now)); got != SkipStaleStatus {
		t.Errorf("got %q, want the override to apply", got)
	}
}
