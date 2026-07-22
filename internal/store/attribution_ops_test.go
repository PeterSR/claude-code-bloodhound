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

func keysOf(m map[string]*SessionPctTotals) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
