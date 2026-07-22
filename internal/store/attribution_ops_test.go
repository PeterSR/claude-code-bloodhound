package store

import (
	"context"
	"testing"
)

// insertTestSession inserts a minimal sessions row. Every NOT NULL column
// besides the ones under test gets an inert placeholder value.
func insertTestSession(t *testing.T, s *Store, uuid, project, parentUUID string) {
	t.Helper()
	_, err := s.DB.Exec(`
		INSERT INTO sessions (
			session_uuid, project, first_ts_unix_ms, last_ts_unix_ms,
			turn_count, raw_tokens, output_tokens, peak_5h_raw_tokens,
			idle_miss_count, rotation_count, restructure_count,
			compaction_count, cold_compaction_count, cache_ttl, models,
			parent_session_uuid, cwd
		) VALUES (?, ?, 0, 0, 1, 100, 10, 100, 0, 0, 0, 0, 0, 'none', '', ?, '')
	`, uuid, project, parentUUID)
	if err != nil {
		t.Fatalf("insert session %s: %v", uuid, err)
	}
}

func insertTestWindow(t *testing.T, s *Store, bucket string, startMS int64) {
	t.Helper()
	_, err := s.DB.Exec(`
		INSERT INTO limit_windows (bucket, start_unix_ms, end_unix_ms)
		VALUES (?, ?, ?)
	`, bucket, startMS, startMS+1000)
	if err != nil {
		t.Fatalf("insert window %s/%d: %v", bucket, startMS, err)
	}
}

func insertTestAttribution(t *testing.T, s *Store, bucket string, windowStartMS int64, sessionUUID, project string, measured, estimated float64) {
	t.Helper()
	_, err := s.DB.Exec(`
		INSERT INTO session_attribution (
			bucket, window_start_unix_ms, session_uuid, project,
			measured_pct, estimated_pct
		) VALUES (?, ?, ?, ?, ?, ?)
	`, bucket, windowStartMS, sessionUUID, project, measured, estimated)
	if err != nil {
		t.Fatalf("insert attribution (%s, %s): %v", sessionUUID, bucket, err)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	s, err := Open(context.Background())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.DB.Close() })
	return s
}

// TestSessionPctTotalsAll_FoldsSubagentsIntoParent is the effective-owner
// rollup test the task calls for: a supervisor session that dispatched two
// subagents must have its total include their spend, and the subagents must
// not appear as separate entries (their cost isn't double-counted, just
// relocated to whoever dispatched them).
//
// It also exercises the two-level (per-window, then per-bucket) aggregation
// SessionPctTotalsAll does: window 1 has the parent AND both subagents
// overlapping (the common case: a supervisor usually dispatches subagents
// within one 5h window), window 2 has the parent alone. A naive flat
// GROUP BY session_uuid replaced with GROUP BY effective_uuid would still
// take MAX() over individual rows and report window 1's peak as 20 (the
// single largest row in it) instead of 40 (the window's pooled total),
// understating a real supervisor's worst window by more than half.
func TestSessionPctTotalsAll_FoldsSubagentsIntoParent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const (
		parent = "11111111-1111-1111-1111-111111111111"
		sub1   = "agent-aaaaaaaaaaaaaaaaa"
		sub2   = "agent-bbbbbbbbbbbbbbbbb"
		other  = "22222222-2222-2222-2222-222222222222"
	)

	insertTestSession(t, s, parent, "proj", "")
	insertTestSession(t, s, sub1, "proj", parent)
	insertTestSession(t, s, sub2, "proj", parent)
	insertTestSession(t, s, other, "proj", "")

	insertTestWindow(t, s, "5h", 1000)
	insertTestWindow(t, s, "5h", 2000)
	insertTestAttribution(t, s, "5h", 1000, parent, "proj", 5, 0)
	insertTestAttribution(t, s, "5h", 1000, sub1, "proj", 20, 0)
	insertTestAttribution(t, s, "5h", 1000, sub2, "proj", 15, 0)
	insertTestAttribution(t, s, "5h", 2000, parent, "proj", 8, 0)
	insertTestAttribution(t, s, "5h", 2000, other, "proj", 50, 0)

	totals, err := s.SessionPctTotalsAll(ctx)
	if err != nil {
		t.Fatalf("SessionPctTotalsAll: %v", err)
	}

	pt, ok := totals[parent]
	if !ok || pt == nil {
		t.Fatalf("no rollup for parent %s (have keys: %v)", parent, keysOf(totals))
	}
	if got, want := pt.FiveHPct, 48.0; got != want {
		t.Errorf("parent FiveHPct = %v, want %v (own 5+8 plus subagents' 20+15)", got, want)
	}
	if got, want := pt.FiveHPeakPct, 40.0; got != want {
		t.Errorf("parent FiveHPeakPct = %v, want %v (window 1's pooled 5+20+15, not a single row's 20)", got, want)
	}
	if got, want := pt.Windows5h, 2; got != want {
		t.Errorf("parent Windows5h = %d, want %d", got, want)
	}

	if _, ok := totals[sub1]; ok {
		t.Errorf("subagent %s must not be a separate key; its cost belongs to the parent's entry", sub1)
	}
	if _, ok := totals[sub2]; ok {
		t.Errorf("subagent %s must not be a separate key", sub2)
	}

	ot, ok := totals[other]
	if !ok || ot == nil || ot.FiveHPct != 50 {
		t.Errorf("unrelated top-level session %s rollup wrong: %+v", other, ot)
	}
}

// TestGroupAttribution_BySessionFoldsSubagentsIntoParent covers the second
// rollup helper named in the spec: the "by session" grouping used by the
// Attribution page and `bloodhound attribution --by session` must key on the
// effective owner too, not list a subagent as a peer of its own parent.
func TestGroupAttribution_BySessionFoldsSubagentsIntoParent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const (
		parent = "33333333-3333-3333-3333-333333333333"
		sub    = "agent-ccccccccccccccccc"
	)

	insertTestSession(t, s, parent, "proj", "")
	insertTestSession(t, s, sub, "proj", parent)

	insertTestWindow(t, s, "week", 5000)
	insertTestAttribution(t, s, "week", 5000, parent, "proj", 10, 0)
	insertTestAttribution(t, s, "week", 5000, sub, "proj", 6, 0)

	groups, err := s.GroupAttribution(ctx, "week", "session", 0)
	if err != nil {
		t.Fatalf("GroupAttribution: %v", err)
	}

	var parentGroup *AttributionGroup
	for i := range groups {
		if groups[i].Key == parent {
			parentGroup = &groups[i]
		}
		if groups[i].Key == sub {
			t.Fatalf("subagent %s appeared as its own group, want it folded into the parent", sub)
		}
	}
	if parentGroup == nil {
		t.Fatalf("no group for parent %s (got %+v)", parent, groups)
	}
	if got, want := parentGroup.Pct, 16.0; got != want {
		t.Errorf("parent group Pct = %v, want %v (own 10 plus subagent's 6)", got, want)
	}
	if got, want := parentGroup.Sessions, 2; got != want {
		t.Errorf("parent group Sessions = %d, want %d (parent + 1 subagent)", got, want)
	}
}

func keysOf(m map[string]*SessionPctTotals) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
