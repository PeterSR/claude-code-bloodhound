package aggregate

import (
	"context"
	"testing"

	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	s, err := store.Open(context.Background())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.DB.Close() })
	return s
}

// insertTurns writes turns for a single session, one row per (tsOffsetMS,
// cwd) pair given, in that order (refreshSessions relies on ts_unix_ms
// ordering, so the offsets should be ascending the way real timestamps
// would be).
func insertTurns(t *testing.T, s *store.Store, uuid string, cwds []string) {
	t.Helper()
	turns := make([]store.TurnRow, len(cwds))
	for i, cwd := range cwds {
		turns[i] = store.TurnRow{
			SessionUUID:  uuid,
			TurnIdx:      i,
			TS:           "2026-07-22T00:00:00Z",
			TSUnixMS:     int64(i) * 1000,
			Model:        "claude",
			InputTokens:  1,
			OutputTokens: 1,
			Project:      "proj",
			Cwd:          cwd,
		}
	}
	if err := s.ReplaceSessionData(context.Background(), store.SessionPersist{
		SessionUUID: uuid,
		Project:     "proj",
		Turns:       turns,
	}); err != nil {
		t.Fatalf("ReplaceSessionData: %v", err)
	}
}

// sessionCwd reads back sessions.cwd directly; refreshSessions is the only
// writer under test here; there's no other package-level helper for a
// single row's cwd.
func sessionCwd(t *testing.T, s *store.Store, uuid string) string {
	t.Helper()
	var cwd string
	if err := s.DB.QueryRowContext(context.Background(),
		`SELECT cwd FROM sessions WHERE session_uuid = ?`, uuid,
	).Scan(&cwd); err != nil {
		t.Fatalf("read back cwd for %s: %v", uuid, err)
	}
	return cwd
}

// TestRefreshSessions_CwdIsMostCommon is the core of the cwd-selection
// decision (see the doc comment on sessAcc.cwdCounts): a session that spent
// most of its turns in one directory should be labelled with that directory
// even though it also touched another one along the way.
func TestRefreshSessions_CwdIsMostCommon(t *testing.T) {
	s := openTestStore(t)
	const uuid = "11111111-1111-1111-1111-111111111111"
	// A, B, A, B, A: A wins 3-2 despite not being first (it is, here, but
	// the point of this case is the count, not the position).
	insertTurns(t, s, uuid, []string{"/a", "/b", "/a", "/b", "/a"})

	if _, err := refreshSessions(context.Background(), s); err != nil {
		t.Fatalf("refreshSessions: %v", err)
	}
	if got, want := sessionCwd(t, s, uuid), "/a"; got != want {
		t.Errorf("cwd = %q, want %q (most common, 3 vs 2)", got, want)
	}
}

// TestRefreshSessions_CwdMostCommonBeatsLastAndFirst pins down that the rule
// is neither "first" nor "last": the session launches in /a, spends most of
// its turns in /b, then returns to /a for the final turn. Both "first" and
// "last" would report /a here; "most common" must report /b.
func TestRefreshSessions_CwdMostCommonBeatsLastAndFirst(t *testing.T) {
	s := openTestStore(t)
	const uuid = "22222222-2222-2222-2222-222222222222"
	insertTurns(t, s, uuid, []string{"/a", "/b", "/b", "/b", "/a"})

	if _, err := refreshSessions(context.Background(), s); err != nil {
		t.Fatalf("refreshSessions: %v", err)
	}
	if got, want := sessionCwd(t, s, uuid), "/b"; got != want {
		t.Errorf("cwd = %q, want %q (most common; first and last turn are both /a)", got, want)
	}
}

// TestRefreshSessions_CwdTiesBreakToFirstSeen covers the deterministic
// tie-break: two directories tied 2-2 must resolve to whichever the session
// visited first, not to map iteration order (which would make the result
// flaky across runs without this rule).
func TestRefreshSessions_CwdTiesBreakToFirstSeen(t *testing.T) {
	s := openTestStore(t)
	const uuid = "33333333-3333-3333-3333-333333333333"
	insertTurns(t, s, uuid, []string{"/b", "/a", "/b", "/a"}) // /b seen first, tied 2-2

	if _, err := refreshSessions(context.Background(), s); err != nil {
		t.Fatalf("refreshSessions: %v", err)
	}
	if got, want := sessionCwd(t, s, uuid), "/b"; got != want {
		t.Errorf("cwd = %q, want %q (tied 2-2, /b seen first)", got, want)
	}
}

// TestRefreshSessions_CwdEmptyTurnsExcludedFromCount is the graceful-empty
// requirement applied to the selection rule itself: turns with no recorded
// cwd (old data, or a transcript ingested before this field existed) must
// not be allowed to outvote a real directory just because there happen to
// be more of them.
func TestRefreshSessions_CwdEmptyTurnsExcludedFromCount(t *testing.T) {
	s := openTestStore(t)
	const uuid = "44444444-4444-4444-4444-444444444444"
	insertTurns(t, s, uuid, []string{"", "", "", "/a"})

	if _, err := refreshSessions(context.Background(), s); err != nil {
		t.Fatalf("refreshSessions: %v", err)
	}
	if got, want := sessionCwd(t, s, uuid), "/a"; got != want {
		t.Errorf("cwd = %q, want %q (the one real directory, not outvoted by 3 blanks)", got, want)
	}
}

// TestRefreshSessions_CwdAllEmptyStaysEmpty: when nothing is known, the
// result must be "", not a fabricated value - the empty-cwd case has to
// render gracefully downstream, not be papered over here.
func TestRefreshSessions_CwdAllEmptyStaysEmpty(t *testing.T) {
	s := openTestStore(t)
	const uuid = "55555555-5555-5555-5555-555555555555"
	insertTurns(t, s, uuid, []string{"", "", ""})

	if _, err := refreshSessions(context.Background(), s); err != nil {
		t.Fatalf("refreshSessions: %v", err)
	}
	if got := sessionCwd(t, s, uuid); got != "" {
		t.Errorf("cwd = %q, want \"\" (no turn ever recorded one)", got)
	}
}
