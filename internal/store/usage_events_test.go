package store

import (
	"context"
	"testing"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/events"
	"github.com/PeterSR/claude-code-bloodhound/internal/usage"
)

func pctOf(v int) *int { return &v }

func recordPct(t *testing.T, s *Store, at time.Time, sess, week int) Observation {
	t.Helper()
	obs, err := s.RecordUsage(context.Background(), 1, usage.Result{
		OK:         true,
		FetchedAt:  at,
		ElapsedS:   8,
		SessionPct: pctOf(sess),
		WeekPct:    pctOf(week),
	}, nil)
	if err != nil {
		t.Fatalf("record usage: %v", err)
	}
	return obs
}

// A window reset is an edge with no persistent state, so it cannot be a level.
// It has to be appended by the transaction that discovered it, which is also
// what makes it work on a cron install with no daemon: `bloodhound poll` goes
// through exactly this path.
func TestRecordUsage_AppendsWindowResetEdge(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	ctx := context.Background()
	s, err := Open(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	base := time.Now().Add(-2 * time.Hour)
	recordPct(t, s, base, 88, 40)

	// No reset yet, so nothing should have been recorded.
	if evs, err := events.Query(ctx, s.DB, events.Filter{}); err != nil {
		t.Fatalf("query: %v", err)
	} else if len(evs) != 0 {
		t.Fatalf("got %d events before any reset, want 0: %+v", len(evs), evs)
	}

	// A large drop in the session bucket is a reset. The weekly bucket keeps
	// climbing, so only one of the two should fire.
	obs := recordPct(t, s, base.Add(time.Hour), 4, 41)
	if !obs.SessionResetDetected {
		t.Fatal("session reset was not detected, so the edge test is not exercising anything")
	}

	evs, err := events.Query(ctx, s.DB, events.Filter{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(evs), evs)
	}
	e := evs[0]
	if e.Kind != "window.reset" {
		t.Errorf("kind = %q, want window.reset", e.Kind)
	}
	if e.Scope.Bucket != "session" {
		t.Errorf("bucket = %q, want session", e.Scope.Bucket)
	}
	if got, ok := e.Detail["pct"].(float64); !ok || int(got) != 4 {
		t.Errorf("detail pct = %v, want 4", e.Detail["pct"])
	}
	if e.PrevState != "" {
		t.Errorf("prev_state = %q, want empty (edges have no previous state)", e.PrevState)
	}
}
