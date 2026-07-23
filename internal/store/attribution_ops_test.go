package store

import (
	"context"
	"testing"
)

// insertTestSession inserts a minimal sessions row with cwd left empty.
// Every NOT NULL column besides the ones under test gets an inert
// placeholder value.
func insertTestSession(t *testing.T, s *Store, uuid, project, parentUUID string) {
	t.Helper()
	insertTestSessionCwd(t, s, uuid, project, parentUUID, "")
}

// insertTestSessionCwd is insertTestSession plus an explicit cwd, for tests
// that need one session's directory to differ from another's (the subagent
// rollup cases: a subagent's own cwd must never surface as its parent
// group's).
func insertTestSessionCwd(t *testing.T, s *Store, uuid, project, parentUUID, cwd string) {
	t.Helper()
	_, err := s.DB.Exec(`
		INSERT INTO sessions (
			session_uuid, project, first_ts_unix_ms, last_ts_unix_ms,
			turn_count, raw_tokens, output_tokens, peak_5h_raw_tokens,
			idle_miss_count, rotation_count, restructure_count,
			compaction_count, cold_compaction_count, cache_ttl, models,
			parent_session_uuid, cwd
		) VALUES (?, ?, 0, 0, 1, 100, 10, 100, 0, 0, 0, 0, 0, 'none', '', ?, ?)
	`, uuid, project, parentUUID, cwd)
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

// TestGroupAttribution_BySessionCwdIsParentsNotSubagents is the cwd version
// of the fold test above: a subagent that genuinely ran in a different
// directory than its dispatcher (verified live: a subagent working in a
// subdirectory of the same repo, or even a different repo entirely) must
// not make its own directory win the rolled-up group's Cwd. The group
// belongs to the parent; its Cwd must too.
func TestGroupAttribution_BySessionCwdIsParentsNotSubagents(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const (
		parent    = "44444444-4444-4444-4444-444444444444"
		sub       = "agent-ddddddddddddddddd"
		parentCwd = "/home/user/projects/myapp"
		subCwd    = "/home/user/projects/myapp/.agent-workspace/helper"
	)

	insertTestSessionCwd(t, s, parent, "proj", "", parentCwd)
	insertTestSessionCwd(t, s, sub, "proj", parent, subCwd)

	insertTestWindow(t, s, "week", 7000)
	insertTestAttribution(t, s, "week", 7000, parent, "proj", 10, 0)
	insertTestAttribution(t, s, "week", 7000, sub, "proj", 6, 0)

	groups, err := s.GroupAttribution(ctx, "week", "session", 0)
	if err != nil {
		t.Fatalf("GroupAttribution: %v", err)
	}

	var parentGroup *AttributionGroup
	for i := range groups {
		if groups[i].Key == parent {
			parentGroup = &groups[i]
		}
	}
	if parentGroup == nil {
		t.Fatalf("no group for parent %s (got %+v)", parent, groups)
	}
	if got := parentGroup.Cwd; got != parentCwd {
		t.Errorf("parent group Cwd = %q, want %q (the parent's own, never the subagent's %q)", got, parentCwd, subCwd)
	}
}

// TestGroupAttribution_ByProjectCwdIsEmpty covers the other half of the
// design decision documented on AttributionGroup.Cwd: a project-keyed group
// never reports a cwd, even in the degenerate case where every session
// under it happens to share one, because the rollup can't tell "everyone
// agrees" from "we only picked one arbitrarily" without inspecting every
// row, and reporting a value at all would invite a caller to trust it in
// the general (multi-directory) case where it's wrong.
func TestGroupAttribution_ByProjectCwdIsEmpty(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const sess = "55555555-5555-5555-5555-555555555555"
	insertTestSessionCwd(t, s, sess, "proj", "", "/home/user/projects/myapp")

	insertTestWindow(t, s, "week", 9000)
	insertTestAttribution(t, s, "week", 9000, sess, "proj", 10, 0)

	groups, err := s.GroupAttribution(ctx, "week", "project", 0)
	if err != nil {
		t.Fatalf("GroupAttribution: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1: %+v", len(groups), groups)
	}
	if got := groups[0].Cwd; got != "" {
		t.Errorf("project-keyed group Cwd = %q, want \"\" (a project can span many directories; see the field doc)", got)
	}
}

// TestSessionPctTotalsAll_CwdIsParentsNotSubagents mirrors the GroupAttribution
// cwd test for the other rollup helper (the one behind /api/sessions and
// /api/sessions/{uuid}): a supervisor's totals must carry its own cwd even
// though the numbers themselves include a subagent that ran elsewhere.
func TestSessionPctTotalsAll_CwdIsParentsNotSubagents(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const (
		parent    = "66666666-6666-6666-6666-666666666666"
		sub       = "agent-eeeeeeeeeeeeeeeee"
		parentCwd = "/home/user/projects/myapp"
		subCwd    = "/home/user/projects/other-repo"
	)

	insertTestSessionCwd(t, s, parent, "proj", "", parentCwd)
	insertTestSessionCwd(t, s, sub, "proj", parent, subCwd)

	insertTestWindow(t, s, "5h", 3000)
	insertTestAttribution(t, s, "5h", 3000, parent, "proj", 5, 0)
	insertTestAttribution(t, s, "5h", 3000, sub, "proj", 3, 0)

	totals, err := s.SessionPctTotalsAll(ctx)
	if err != nil {
		t.Fatalf("SessionPctTotalsAll: %v", err)
	}
	pt, ok := totals[parent]
	if !ok || pt == nil {
		t.Fatalf("no rollup for parent %s (have keys: %v)", parent, keysOf(totals))
	}
	if got := pt.Cwd; got != parentCwd {
		t.Errorf("parent totals Cwd = %q, want %q (never the subagent's %q)", got, parentCwd, subCwd)
	}
}

// TestSessionPctTotalsAll_CwdEmptyWhenUnknown covers the graceful-empty
// requirement: a session whose transcript rotated off disk before cwd
// existed as a column has "" in the sessions table (the migration's
// documented default), and that must surface as "" here too rather than
// erroring or fabricating a value.
func TestSessionPctTotalsAll_CwdEmptyWhenUnknown(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const sess = "77777777-7777-7777-7777-777777777777"
	insertTestSession(t, s, sess, "proj", "") // cwd == "" by default

	insertTestWindow(t, s, "week", 11000)
	insertTestAttribution(t, s, "week", 11000, sess, "proj", 4, 0)

	totals, err := s.SessionPctTotalsAll(ctx)
	if err != nil {
		t.Fatalf("SessionPctTotalsAll: %v", err)
	}
	pt, ok := totals[sess]
	if !ok || pt == nil {
		t.Fatalf("no rollup for session %s", sess)
	}
	if got := pt.Cwd; got != "" {
		t.Errorf("Cwd = %q, want \"\" (unknown, not fabricated)", got)
	}
}

// TestGroupAttribution_ByCwdGroupsUnderParentDirectory is the by=="cwd"
// counterpart of TestGroupAttribution_BySessionCwdIsParentsNotSubagents: a
// supervisor that dispatched a subagent into a different directory must
// still roll up into ONE group keyed on its own directory, not two (one per
// literal cwd), since the whole point of --by cwd is "how much did this
// directory cost", and the subagent's cost was incurred on the
// supervisor's behalf.
func TestGroupAttribution_ByCwdGroupsUnderParentDirectory(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const (
		parent    = "88888888-8888-8888-8888-888888888888"
		sub       = "agent-fffffffffffffffff"
		parentCwd = "/home/user/projects/myapp"
		subCwd    = "/home/user/projects/myapp/.agent-workspace/helper"
	)

	insertTestSessionCwd(t, s, parent, "proj", "", parentCwd)
	insertTestSessionCwd(t, s, sub, "proj", parent, subCwd)

	insertTestWindow(t, s, "week", 13000)
	insertTestAttribution(t, s, "week", 13000, parent, "proj", 10, 0)
	insertTestAttribution(t, s, "week", 13000, sub, "proj", 6, 0)

	groups, err := s.GroupAttribution(ctx, "week", "cwd", 0)
	if err != nil {
		t.Fatalf("GroupAttribution: %v", err)
	}

	var parentGroup, subGroup *AttributionGroup
	for i := range groups {
		switch groups[i].Key {
		case parentCwd:
			parentGroup = &groups[i]
		case subCwd:
			subGroup = &groups[i]
		}
	}
	if subGroup != nil {
		t.Fatalf("subagent's own directory %q appeared as its own group, want everything folded under the parent's %q: %+v", subCwd, parentCwd, groups)
	}
	if parentGroup == nil {
		t.Fatalf("no group for parent cwd %q (got %+v)", parentCwd, groups)
	}
	if got, want := parentGroup.Pct, 16.0; got != want {
		t.Errorf("parent cwd group Pct = %v, want %v (own 10 plus subagent's 6)", got, want)
	}
	if got, want := parentGroup.Sessions, 2; got != want {
		t.Errorf("parent cwd group Sessions = %d, want %d (parent + 1 subagent)", got, want)
	}
	if got := parentGroup.Cwd; got != parentCwd {
		t.Errorf("parent cwd group Cwd = %q, want %q (populated the same as Key for a cwd-keyed group)", got, parentCwd)
	}
}

// TestGroupAttribution_ByCwdUnknownBucketDistinctFromUnattributed is the
// bucket the task calls for: a session_attribution row that belongs to a
// real, known session whose cwd was never captured must land in its own
// explicit UnknownCwd bucket, never silently dropped and never merged with
// the unattributed sentinel (key ""), which is a different fact entirely
// (meter movement no turn of ours explains, unconnected to any session).
func TestGroupAttribution_ByCwdUnknownBucketDistinctFromUnattributed(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const knownNoCwd = "99999999-9999-9999-9999-999999999999"
	insertTestSession(t, s, knownNoCwd, "proj", "") // cwd == "" by default

	insertTestWindow(t, s, "week", 15000)
	insertTestAttribution(t, s, "week", 15000, knownNoCwd, "proj", 7, 0)
	// The unattributed remainder: an empty session_uuid, same sentinel
	// attribute.Unattributed uses everywhere else.
	insertTestAttribution(t, s, "week", 15000, "", "", 3, 0)

	groups, err := s.GroupAttribution(ctx, "week", "cwd", 0)
	if err != nil {
		t.Fatalf("GroupAttribution: %v", err)
	}

	var unknownGroup, unattributedGroup *AttributionGroup
	for i := range groups {
		switch groups[i].Key {
		case UnknownCwd:
			unknownGroup = &groups[i]
		case "":
			unattributedGroup = &groups[i]
		}
	}
	if unknownGroup == nil {
		t.Fatalf("no UnknownCwd bucket in groups: %+v", groups)
	}
	if unattributedGroup == nil {
		t.Fatalf("no unattributed (key \"\") bucket in groups: %+v", groups)
	}
	if got, want := unknownGroup.Pct, 7.0; got != want {
		t.Errorf("UnknownCwd bucket Pct = %v, want %v (the known session's 7)", got, want)
	}
	if got, want := unknownGroup.Sessions, 1; got != want {
		t.Errorf("UnknownCwd bucket Sessions = %d, want %d (we know exactly which session, so it counts)", got, want)
	}
	if got := unknownGroup.Cwd; got != "" {
		t.Errorf("UnknownCwd bucket's own Cwd field = %q, want \"\" (there is no directory to report, only Key distinguishes it)", got)
	}
	if got, want := unattributedGroup.Pct, 3.0; got != want {
		t.Errorf("unattributed bucket Pct = %v, want %v", got, want)
	}
	if got, want := unattributedGroup.Sessions, 0; got != want {
		t.Errorf("unattributed bucket Sessions = %d, want %d (a window property, not a session)", got, want)
	}
}

// TestGroupAttribution_ByCwdRealDirectoryPopulatesCwdField covers the
// ordinary case: a session with a real, captured cwd groups under that
// literal path, and the row's own Cwd field carries the same value as Key
// (per the task's "populate cwd on the returned group rows for this
// grouping, where it is the key").
func TestGroupAttribution_ByCwdRealDirectoryPopulatesCwdField(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const (
		sess = "aaaaaaaa-1111-1111-1111-111111111111"
		cwd  = "/home/user/projects/otherapp"
	)
	insertTestSessionCwd(t, s, sess, "proj", "", cwd)

	insertTestWindow(t, s, "week", 17000)
	insertTestAttribution(t, s, "week", 17000, sess, "proj", 12, 0)

	groups, err := s.GroupAttribution(ctx, "week", "cwd", 0)
	if err != nil {
		t.Fatalf("GroupAttribution: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1: %+v", len(groups), groups)
	}
	if got := groups[0].Key; got != cwd {
		t.Errorf("group Key = %q, want %q", got, cwd)
	}
	if got := groups[0].Cwd; got != cwd {
		t.Errorf("group Cwd = %q, want %q (same value as Key)", got, cwd)
	}
}

// TestWindowSlices_CwdIsParentsNotSubagents guards the WindowSlices half of
// the same effective-owner resolution: the web stacked-window chart keys
// its by=="cwd" slices on this field (see buildAttrWindows), so a
// subagent's own cwd must never leak into it either.
func TestWindowSlices_CwdIsParentsNotSubagents(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const (
		parent    = "bbbbbbbb-2222-2222-2222-222222222222"
		sub       = "agent-ggggggggggggggggg"
		parentCwd = "/home/user/projects/myapp"
		subCwd    = "/home/user/projects/other-repo"
	)
	insertTestSessionCwd(t, s, parent, "proj", "", parentCwd)
	insertTestSessionCwd(t, s, sub, "proj", parent, subCwd)

	insertTestWindow(t, s, "5h", 19000)
	insertTestAttribution(t, s, "5h", 19000, parent, "proj", 5, 0)
	insertTestAttribution(t, s, "5h", 19000, sub, "proj", 3, 0)

	rows, err := s.WindowSlices(ctx, "5h", 0)
	if err != nil {
		t.Fatalf("WindowSlices: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
	}
	for _, r := range rows {
		if r.Cwd != parentCwd {
			t.Errorf("row for session %q has Cwd = %q, want %q (never the subagent's %q)", r.SessionUUID, r.Cwd, parentCwd, subCwd)
		}
	}
}

// TestSessionPctWindows_FoldsSubagentsIntoParent is the fix the task calls
// for: SessionPctWindows must fold a subagent's spend into whichever session
// dispatched it, the same effective-owner rule SessionPctTotalsAll already
// applies, or the two disagree about the same session's cost. Mirrors
// TestSessionPctTotalsAll_FoldsSubagentsIntoParent's shape (one window with
// the parent and both subagents overlapping, a second with the parent
// alone) so the two can be checked against each other directly.
func TestSessionPctWindows_FoldsSubagentsIntoParent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const (
		parent = "66666666-6666-6666-6666-666666666666"
		sub1   = "agent-eeeeeeeeeeeeeeeee"
		sub2   = "agent-fffffffffffffffff"
	)

	insertTestSession(t, s, parent, "proj", "")
	insertTestSession(t, s, sub1, "proj", parent)
	insertTestSession(t, s, sub2, "proj", parent)

	insertTestWindow(t, s, "5h", 1000)
	insertTestWindow(t, s, "5h", 2000)
	insertTestAttribution(t, s, "5h", 1000, parent, "proj", 5, 0)
	insertTestAttribution(t, s, "5h", 1000, sub1, "proj", 20, 0)
	insertTestAttribution(t, s, "5h", 1000, sub2, "proj", 15, 0)
	insertTestAttribution(t, s, "5h", 2000, parent, "proj", 8, 0)

	rows, wins, err := s.SessionPctWindows(ctx, parent, "5h")
	if err != nil {
		t.Fatalf("SessionPctWindows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d window rows, want 2 (one per window, subagents folded rather than exploded): %+v", len(rows), rows)
	}
	if len(wins) != len(rows) {
		t.Fatalf("got %d window metadata rows for %d attribution rows, want them to match", len(wins), len(rows))
	}

	if got, want := rows[0].WindowStartUnixMS, int64(1000); got != want {
		t.Fatalf("rows[0].WindowStartUnixMS = %d, want %d (oldest first)", got, want)
	}
	if got, want := rows[0].MeasuredPct, 40.0; got != want {
		t.Errorf("window 1 pct = %v, want %v (parent's 5 plus subagents' 20+15)", got, want)
	}
	if got, want := rows[1].MeasuredPct, 8.0; got != want {
		t.Errorf("window 2 pct = %v, want %v (parent alone, no subagents in this window)", got, want)
	}

	// The acceptance test from the spec: the array must sum to the same
	// figure SessionPctTotalsAll's folded total reports.
	totals, err := s.SessionPctTotalsAll(ctx)
	if err != nil {
		t.Fatalf("SessionPctTotalsAll: %v", err)
	}
	var sum float64
	for _, r := range rows {
		sum += r.MeasuredPct + r.EstimatedPct
	}
	if got, want := sum, totals[parent].FiveHPct; got != want {
		t.Errorf("sum of SessionPctWindows rows = %v, want %v (SessionPctTotalsAll's folded FiveHPct)", got, want)
	}
}

// TestSessionPctWindows_SubagentOwnUUIDStaysUnfolded guards the other half
// of the fix: querying a subagent BY ITS OWN uuid must still return its own
// raw, unfolded row, not the row folded into its parent (which wouldn't even
// be keyed by the subagent's uuid). This is the one reachability path
// "attribution windows --slices" and "attribution --by session" promise in
// their own --help text after they fold a subagent away.
func TestSessionPctWindows_SubagentOwnUUIDStaysUnfolded(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const (
		parent = "77777777-7777-7777-7777-777777777777"
		sub    = "agent-ddddddddddddddddd"
	)
	insertTestSession(t, s, parent, "proj", "")
	insertTestSession(t, s, sub, "proj", parent)

	insertTestWindow(t, s, "5h", 3000)
	insertTestAttribution(t, s, "5h", 3000, parent, "proj", 5, 0)
	insertTestAttribution(t, s, "5h", 3000, sub, "proj", 20, 0)

	rows, _, err := s.SessionPctWindows(ctx, sub, "5h")
	if err != nil {
		t.Fatalf("SessionPctWindows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows for the subagent's own uuid, want 1 (its own row, not the parent's folded one): %+v", len(rows), rows)
	}
	if got, want := rows[0].MeasuredPct, 20.0; got != want {
		t.Errorf("subagent's own pct = %v, want %v (its own 20, not folded with the parent's 5)", got, want)
	}
}

// TestGroupAttribution_ByProjectPeakSumsWindowBeforeMax pins the bug 2 fix:
// two unrelated sessions sharing one project and one window must have their
// shares SUMMED before the group's peak takes the max across windows. Before
// the fix, PeakPct was MAX() over raw session_attribution rows, so it picked
// the single largest contributing session (30) rather than the window's own
// total (55) - understating the peak by whatever the other sessions in that
// window contributed alongside it, exactly the live symptom the task
// reported (a project with one window and 116 sessions inside it reporting
// a peak far below its total).
func TestGroupAttribution_ByProjectPeakSumsWindowBeforeMax(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const (
		sessA = "aaaaaaaa-0000-0000-0000-000000000001"
		sessB = "bbbbbbbb-0000-0000-0000-000000000002"
	)
	insertTestSession(t, s, sessA, "proj", "")
	insertTestSession(t, s, sessB, "proj", "")

	insertTestWindow(t, s, "week", 1000)
	insertTestWindow(t, s, "week", 2000)
	// Window 1: two sessions share it, pooled total 55 - the true peak.
	insertTestAttribution(t, s, "week", 1000, sessA, "proj", 30, 0)
	insertTestAttribution(t, s, "week", 1000, sessB, "proj", 25, 0)
	// Window 2: one session alone, well under window 1's pooled total.
	insertTestAttribution(t, s, "week", 2000, sessA, "proj", 10, 0)

	groups, err := s.GroupAttribution(ctx, "week", "project", 0)
	if err != nil {
		t.Fatalf("GroupAttribution: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1: %+v", len(groups), groups)
	}
	g := groups[0]
	if got, want := g.Pct, 65.0; got != want {
		t.Errorf("Pct = %v, want %v (30+25+10)", got, want)
	}
	if got, want := g.PeakPct, 55.0; got != want {
		t.Errorf("PeakPct = %v, want %v (window 1's pooled 30+25, not the single largest row 30)", got, want)
	}
}

// TestGroupAttribution_ByCwdPeakSumsWindowBeforeMax is the cwd counterpart of
// the project test above: two sessions that happen to share a directory
// (not a parent/subagent pair - just two separate top-level sessions run
// from the same cwd) must have their shares summed per window before the
// group's peak maxes across windows.
func TestGroupAttribution_ByCwdPeakSumsWindowBeforeMax(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const (
		sessA = "cccccccc-0000-0000-0000-000000000003"
		sessB = "dddddddd-0000-0000-0000-000000000004"
		cwd   = "/home/user/projects/myapp"
	)
	insertTestSessionCwd(t, s, sessA, "proj", "", cwd)
	insertTestSessionCwd(t, s, sessB, "proj", "", cwd)

	insertTestWindow(t, s, "week", 1000)
	insertTestWindow(t, s, "week", 2000)
	insertTestAttribution(t, s, "week", 1000, sessA, "proj", 12, 0)
	insertTestAttribution(t, s, "week", 1000, sessB, "proj", 9, 0)
	insertTestAttribution(t, s, "week", 2000, sessA, "proj", 5, 0)

	groups, err := s.GroupAttribution(ctx, "week", "cwd", 0)
	if err != nil {
		t.Fatalf("GroupAttribution: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1: %+v", len(groups), groups)
	}
	g := groups[0]
	if got, want := g.Pct, 26.0; got != want {
		t.Errorf("Pct = %v, want %v (12+9+5)", got, want)
	}
	if got, want := g.PeakPct, 21.0; got != want {
		t.Errorf("PeakPct = %v, want %v (window 1's pooled 12+9, not the single largest row 12)", got, want)
	}
}

// TestGroupAttribution_SingleWindowPeakEqualsPctForProjectAndCwd is the
// sharpest form of the bug 2 fix: with exactly one window in range, a
// group's peak is that window's whole share by definition, no matter how
// many distinct sessions contributed to it. Covers both by=="project" and
// by=="cwd" against the same fixture.
func TestGroupAttribution_SingleWindowPeakEqualsPctForProjectAndCwd(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const (
		sessA = "eeeeeeee-0000-0000-0000-000000000005"
		sessB = "ffffffff-0000-0000-0000-000000000006"
		cwd   = "/home/user/projects/myapp"
	)
	insertTestSessionCwd(t, s, sessA, "proj", "", cwd)
	insertTestSessionCwd(t, s, sessB, "proj", "", cwd)

	insertTestWindow(t, s, "week", 1000)
	insertTestAttribution(t, s, "week", 1000, sessA, "proj", 30, 2)
	insertTestAttribution(t, s, "week", 1000, sessB, "proj", 25, 1)

	for _, by := range []string{"project", "cwd"} {
		groups, err := s.GroupAttribution(ctx, "week", by, 0)
		if err != nil {
			t.Fatalf("GroupAttribution(%s): %v", by, err)
		}
		if len(groups) != 1 {
			t.Fatalf("by=%s: groups = %d, want 1: %+v", by, len(groups), groups)
		}
		g := groups[0]
		if g.PeakPct != g.Pct {
			t.Errorf("by=%s: PeakPct = %v, Pct = %v, want equal (one window in range)", by, g.PeakPct, g.Pct)
		}
		if got, want := g.Pct, 58.0; got != want {
			t.Errorf("by=%s: Pct = %v, want %v (30+2+25+1)", by, got, want)
		}
	}
}

func keysOf(m map[string]*SessionPctTotals) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestGroupAttribution_BySessionPeakSumsSupervisorAndSubagents is the
// by=="session" counterpart of the two tests above. A supervisor and the
// subagents it dispatched all fold into one effective-owner group and each
// contributes its own row to the same window, so the group's peak has to sum
// them before taking the max, exactly as project and cwd do.
//
// This grouping was deliberately left on a plain MAX() over raw rows when the
// project/cwd fix landed. That understated the peak by whatever the subagents
// contributed alongside their supervisor, and produced a visibly
// self-contradictory pair on the Attribution page: a real database showed one
// owner with 115 rows in a single window summing to 37.45% but reporting a
// 9.3% peak, the supervisor's own row. With one window in range the peak is
// the total by definition.
func TestGroupAttribution_BySessionPeakSumsSupervisorAndSubagents(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const (
		parent = "12121212-1212-1212-1212-121212121212"
		subA   = "agent-fff0000000000000a"
		subB   = "agent-fff0000000000000b"
	)
	insertTestSession(t, s, parent, "proj", "")
	insertTestSession(t, s, subA, "proj", parent)
	insertTestSession(t, s, subB, "proj", parent)

	insertTestWindow(t, s, "week", 1000)
	insertTestWindow(t, s, "week", 2000)
	// Window 1: supervisor plus both subagents, pooled 9+4+3 = 16.
	insertTestAttribution(t, s, "week", 1000, parent, "proj", 9, 0)
	insertTestAttribution(t, s, "week", 1000, subA, "proj", 4, 0)
	insertTestAttribution(t, s, "week", 1000, subB, "proj", 3, 0)
	// Window 2: supervisor alone, larger than any single row in window 1 but
	// smaller than window 1's pooled total. This is what separates the fix
	// from the bug: the old MAX() over raw rows would report 10 here.
	insertTestAttribution(t, s, "week", 2000, parent, "proj", 10, 0)

	groups, err := s.GroupAttribution(ctx, "week", "session", 0)
	if err != nil {
		t.Fatalf("GroupAttribution: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1 (subagents fold into their supervisor): %+v", len(groups), groups)
	}
	g := groups[0]
	if got, want := g.Pct, 26.0; got != want {
		t.Errorf("Pct = %v, want %v (9+4+3+10)", got, want)
	}
	if got, want := g.PeakPct, 16.0; got != want {
		t.Errorf("PeakPct = %v, want %v (window 1's pooled 9+4+3, not window 2's single row 10)", got, want)
	}
	if g.PeakPct > g.Pct {
		t.Errorf("PeakPct %v exceeds Pct %v, which is impossible", g.PeakPct, g.Pct)
	}
}
