package store

import (
	"testing"
	"time"
)

func aWakeup(session, bucket string, now time.Time, due time.Duration) SessionWakeup {
	return SessionWakeup{
		SessionUUID: session,
		PID:         4242,
		Cwd:         "/home/dev/myapp",
		Bucket:      bucket,
		Reason:      "Budget: myapp has 4.0% of its 20% 5h allowance left.",
		ArmedMS:     now.UnixMilli(),
		DueMS:       now.Add(due).UnixMilli(),
		ExpireMS:    now.Add(due + 6*time.Hour).UnixMilli(),
	}
}

func TestArmingTwiceForOneWindowIsOnePromise(t *testing.T) {
	// Pressure crossing twice inside one window is one stop with one far
	// side. Two rows would mean the session is written to twice when it
	// reopens, which is the one outcome worse than not writing at all.
	s, ctx := budgetStore(t)
	now := time.Now()

	if _, err := s.ArmWakeup(ctx, aWakeup("session-abc12345", "session", now, 90*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// The second reading has the better deadline: a poll landing between two
	// warnings can revise when the window actually reopens.
	later := aWakeup("session-abc12345", "session", now, 2*time.Hour)
	if _, err := s.ArmWakeup(ctx, later); err != nil {
		t.Fatal(err)
	}

	pending, err := s.PendingWakeups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want one promise", len(pending))
	}
	if pending[0].DueMS != later.DueMS {
		t.Errorf("due = %d, want the refreshed deadline %d", pending[0].DueMS, later.DueMS)
	}
}

func TestTwoWindowsAreTwoPromises(t *testing.T) {
	// A session can be warned about both windows at once, and they reopen at
	// different times. Collapsing them would lose the later one, which is the
	// one that actually governs when work can restart.
	s, ctx := budgetStore(t)
	now := time.Now()

	for _, b := range []string{"session", "week"} {
		if _, err := s.ArmWakeup(ctx, aWakeup("session-abc12345", b, now, time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := s.PendingWakeups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending = %d, want one per window", len(pending))
	}
}

func TestAResolvedPromiseDoesNotBlockTheNextOne(t *testing.T) {
	// The uniqueness is on pending rows only. Once a promise is kept it is
	// history, and the same session hitting the same wall tomorrow is a new
	// promise rather than a conflict.
	s, ctx := budgetStore(t)
	now := time.Now()

	id, err := s.ArmWakeup(ctx, aWakeup("session-abc12345", "session", now, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveWakeup(ctx, id, WakeupDelivered, now.UnixMilli(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ArmWakeup(ctx, aWakeup("session-abc12345", "session", now, 5*time.Hour)); err != nil {
		t.Fatalf("a second promise after the first was kept: %v", err)
	}

	pending, err := s.PendingWakeups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want only the new one", len(pending))
	}
	all, err := s.RecentWakeups(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("history = %d rows, want the kept promise still on record", len(all))
	}
}

func TestTouchKeepsAPromisePending(t *testing.T) {
	// A socket that did not answer this minute is not a promise to give up
	// on. The next tick is sixty seconds away and expire_ms is what ends the
	// retrying.
	s, ctx := budgetStore(t)
	now := time.Now()

	id, err := s.ArmWakeup(ctx, aWakeup("session-abc12345", "session", now, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.TouchWakeup(ctx, id, "unreachable"); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := s.PendingWakeups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want the promise still waiting", len(pending))
	}
	if pending[0].Attempts != 3 {
		t.Errorf("attempts = %d, want 3", pending[0].Attempts)
	}
}

func TestPruningNeverDropsAPromiseStillWaiting(t *testing.T) {
	// The one row in this table that is not history. A pending promise older
	// than the retention window is a machine that was suspended, not a stale
	// record, and deleting it silently breaks the only thing bloodhound ever
	// promised to do.
	s, ctx := budgetStore(t)
	old := time.Now().Add(-30 * 24 * time.Hour)

	if _, err := s.ArmWakeup(ctx, aWakeup("session-abc12345", "session", old, time.Hour)); err != nil {
		t.Fatal(err)
	}
	kept, err := s.ArmWakeup(ctx, aWakeup("session-def67890", "week", old, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveWakeup(ctx, kept, WakeupDelivered, old.UnixMilli(), ""); err != nil {
		t.Fatal(err)
	}

	n, err := s.PruneWakeups(ctx, time.Now().Add(-14*24*time.Hour).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("pruned %d rows, want only the resolved one", n)
	}
	pending, err := s.PendingWakeups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Error("a promise still waiting was pruned as history")
	}
}

func TestCancelRetiresPendingPromisesOnly(t *testing.T) {
	s, ctx := budgetStore(t)
	now := time.Now()

	for _, sess := range []string{"session-abc12345", "session-def67890"} {
		if _, err := s.ArmWakeup(ctx, aWakeup(sess, "session", now, time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.CancelWakeups(ctx, "session-abc12345", now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("cancelled %d, want just the one session's", n)
	}
	pending, err := s.PendingWakeups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].SessionUUID != "session-def67890" {
		t.Errorf("pending = %+v, want the other session untouched", pending)
	}
}
