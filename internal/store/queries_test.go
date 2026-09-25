package store

import (
	"context"
	"testing"
)

// insertTestSessionWithTurns inserts a sessions row with a caller-chosen
// turn_count, so a test can tell a parent's own turns apart from what its
// subagents summed to. insertTestSession (attribution_ops_test.go) always
// uses 1 and isn't enough for that.
func insertTestSessionWithTurns(t *testing.T, s *Store, uuid, project, parentUUID string, turnCount int) {
	t.Helper()
	_, err := s.DB.Exec(`
		INSERT INTO sessions (
			session_uuid, project, first_ts_unix_ms, last_ts_unix_ms,
			turn_count, raw_tokens, output_tokens, peak_5h_raw_tokens,
			idle_miss_count, rotation_count, restructure_count,
			compaction_count, cold_compaction_count, cache_ttl, models,
			parent_session_uuid, cwd
		) VALUES (?, ?, 0, 0, ?, 100, 10, 100, 0, 0, 0, 0, 0, 'none', '', ?, '')
	`, uuid, project, turnCount, parentUUID)
	if err != nil {
		t.Fatalf("insert session %s: %v", uuid, err)
	}
}

// TestListSessions_ExcludesSubagentsAndReportsCount is the Regression 1 fix:
// a supervisor session that dispatched subagents must appear exactly once,
// carrying a count of what it dispatched, and the subagents themselves must
// not be separate rows flooding the list.
func TestListSessions_ExcludesSubagentsAndReportsCount(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const (
		parent = "66666666-6666-6666-6666-666666666666"
		sub1   = "agent-ffffffffffffffffff"
		sub2   = "agent-gggggggggggggggggg"
		other  = "77777777-7777-7777-7777-777777777777"
	)

	insertTestSessionWithTurns(t, s, parent, "proj", "", 10)
	insertTestSessionWithTurns(t, s, sub1, "proj", parent, 7)
	insertTestSessionWithTurns(t, s, sub2, "proj", parent, 5)
	insertTestSessionWithTurns(t, s, other, "proj", "", 3)

	rows, err := s.ListSessions(ctx, 1)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("ListSessions returned %d rows, want 2 (parent + other, subagents excluded): %+v", len(rows), rows)
	}

	var parentRow, otherRow *SessionListRow
	for i := range rows {
		switch rows[i].SessionUUID {
		case parent:
			parentRow = &rows[i]
		case other:
			otherRow = &rows[i]
		case sub1, sub2:
			t.Fatalf("subagent %s must not appear in ListSessions", rows[i].SessionUUID)
		}
	}
	if parentRow == nil {
		t.Fatalf("no row for parent %s", parent)
	}
	if otherRow == nil {
		t.Fatalf("no row for unrelated top-level session %s", other)
	}

	if got, want := parentRow.SubagentCount, 2; got != want {
		t.Errorf("parent SubagentCount = %d, want %d", got, want)
	}
	if got, want := parentRow.SubagentTurnCount, 12; got != want {
		t.Errorf("parent SubagentTurnCount = %d, want %d (sub1's 7 plus sub2's 5)", got, want)
	}
	if got, want := parentRow.TurnCount, 10; got != want {
		t.Errorf("parent TurnCount = %d, want %d (own turns, unaffected by subagent rollup)", got, want)
	}

	if got, want := otherRow.SubagentCount, 0; got != want {
		t.Errorf("unrelated session SubagentCount = %d, want %d", got, want)
	}
}
