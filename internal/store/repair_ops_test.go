package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---------------------------------------------------------------------
// collapseRuns: pure logic, no DB
// ---------------------------------------------------------------------

// turnAt builds a minimal turn for collapse tests. Tests override only the
// fields they care about via the trailing mutators.
func turnAt(idx int, tsMS int64, model string, in, out int) TurnRow {
	return TurnRow{
		SessionUUID:    "s",
		TurnIdx:        idx,
		TS:             "t",
		TSUnixMS:       tsMS,
		Model:          model,
		InputTokens:    in,
		OutputTokens:   out,
		GapS:           float64(idx), // distinct per input turn so tests can tell which one survived
		Classification: "normal",
		Project:        "proj",
		SourcePathHash: "hash",
	}
}

func TestCollapseRunsMergesADuplicateTriple(t *testing.T) {
	// Three lines from one API response: identical tuple, a few ms apart.
	// The 4th turn is a different response entirely (different tuple) and
	// must survive untouched.
	turns := []TurnRow{
		turnAt(0, 1000, "m", 10, 5),
		turnAt(1, 1010, "m", 10, 5),
		turnAt(2, 1020, "m", 10, 5),
		turnAt(3, 1030, "m2", 99, 1),
	}
	turns[1].PostCompact = true // only the middle row of the run carries it

	out, runs := collapseRuns(turns, 60)
	if runs != 1 {
		t.Fatalf("runsCollapsed = %d, want 1", runs)
	}
	if len(out) != 2 {
		t.Fatalf("len(out) = %d, want 2 (1 collapsed + 1 untouched)", len(out))
	}

	collapsed := out[0]
	if collapsed.TSUnixMS != 1020 {
		t.Errorf("ts_unix_ms = %d, want 1020 (last row's)", collapsed.TSUnixMS)
	}
	if collapsed.GapS != 0 {
		t.Errorf("gap_s = %v, want 0 (first row's, turnAt(0,...) sets GapS=0)", collapsed.GapS)
	}
	if !collapsed.PostCompact {
		t.Errorf("post_compact = false, want true (OR'd across the run)")
	}

	if out[1] != turns[3] {
		t.Errorf("untouched turn changed: got %+v, want %+v", out[1], turns[3])
	}
}

func TestCollapseRunsRespectsWindow(t *testing.T) {
	// Same tuple, but 61s apart: outside a 60s window, so these are two
	// separate real turns that happen to cost the same, not a duplicate.
	turns := []TurnRow{
		turnAt(0, 0, "m", 10, 5),
		turnAt(1, 61_000, "m", 10, 5),
	}
	out, runs := collapseRuns(turns, 60)
	if runs != 0 {
		t.Fatalf("runsCollapsed = %d, want 0 (61s > 60s window)", runs)
	}
	if len(out) != 2 {
		t.Fatalf("len(out) = %d, want 2 (nothing collapsed)", len(out))
	}
}

func TestCollapseRunsExactlyAtWindowBoundaryMerges(t *testing.T) {
	turns := []TurnRow{
		turnAt(0, 0, "m", 10, 5),
		turnAt(1, 60_000, "m", 10, 5), // exactly 60s: <=, not <
	}
	_, runs := collapseRuns(turns, 60)
	if runs != 1 {
		t.Fatalf("runsCollapsed = %d, want 1 (60s == window is inclusive)", runs)
	}
}

func TestCollapseRunsDifferentTupleNeverMerges(t *testing.T) {
	// Same timestamp, different token counts: two genuinely different
	// responses that happen to land in the same millisecond.
	turns := []TurnRow{
		turnAt(0, 1000, "m", 10, 5),
		turnAt(1, 1000, "m", 10, 6),
	}
	_, runs := collapseRuns(turns, 60)
	if runs != 0 {
		t.Fatalf("runsCollapsed = %d, want 0 (tuples differ)", runs)
	}
}

func TestCollapseRunsSingleTurnSessionIsANoOp(t *testing.T) {
	turns := []TurnRow{turnAt(0, 1000, "m", 10, 5)}
	out, runs := collapseRuns(turns, 60)
	if runs != 0 || len(out) != 1 {
		t.Fatalf("got out=%v runs=%d, want unchanged single turn", out, runs)
	}
}

func TestRenumberTurnsIsDenseFromZero(t *testing.T) {
	in := []TurnRow{turnAt(5, 0, "m", 1, 1), turnAt(9, 1, "m", 2, 2), turnAt(40, 2, "m", 3, 3)}
	out := renumberTurns(in)
	for i, tr := range out {
		if tr.TurnIdx != i {
			t.Errorf("out[%d].TurnIdx = %d, want %d", i, tr.TurnIdx, i)
		}
	}
}

// ---------------------------------------------------------------------
// NextBackupPath: name selection, no DB needed (only s.Path is read)
// ---------------------------------------------------------------------

func TestNextBackupPathNoCollisionReturnsBaseName(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Path: filepath.Join(dir, "bloodhound.db")}
	at := time.Date(2026, 7, 22, 12, 26, 0, 0, time.UTC)

	got, err := s.NextBackupPath(at)
	if err != nil {
		t.Fatalf("NextBackupPath: %v", err)
	}
	if want := s.BackupPath(at); got != want {
		t.Errorf("got %q, want %q (nothing taken yet, should be the plain minute-resolution name)", got, want)
	}
}

func TestNextBackupPathCollisionAppendsNumericSuffix(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Path: filepath.Join(dir, "bloodhound.db")}
	at := time.Date(2026, 7, 22, 12, 26, 0, 0, time.UTC)
	base := s.BackupPath(at)

	// Claim the base name, as a first --apply within this minute would.
	if err := os.WriteFile(base, nil, 0o644); err != nil {
		t.Fatalf("seed base backup: %v", err)
	}
	got, err := s.NextBackupPath(at)
	if err != nil {
		t.Fatalf("NextBackupPath: %v", err)
	}
	if got != base+".2" {
		t.Errorf("got %q, want %q (base taken, first free suffix is .2)", got, base+".2")
	}

	// Claim .2 too, as a second --apply in the same minute would.
	if err := os.WriteFile(got, nil, 0o644); err != nil {
		t.Fatalf("seed .2 backup: %v", err)
	}
	got2, err := s.NextBackupPath(at)
	if err != nil {
		t.Fatalf("NextBackupPath: %v", err)
	}
	if got2 != base+".3" {
		t.Errorf("got %q, want %q (base and .2 both taken, next free is .3)", got2, base+".3")
	}

	// An older-style manual backup at a different minute-resolution
	// timestamp (the exact shape a real user already has on disk) must
	// neither collide with nor influence this search.
	older := s.Path + ".backup-20260604-0923"
	if err := os.WriteFile(older, nil, 0o644); err != nil {
		t.Fatalf("seed older-style backup: %v", err)
	}
	if got3, err := s.NextBackupPath(at); err != nil || got3 != base+".3" {
		t.Errorf("older-style backup at a different timestamp changed the result: got %q, err %v, want %q, nil", got3, err, base+".3")
	}
}

func TestNextBackupPathGivesUpAfterBoundedAttempts(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Path: filepath.Join(dir, "bloodhound.db")}
	at := time.Date(2026, 7, 22, 12, 26, 0, 0, time.UTC)
	base := s.BackupPath(at)

	// Claim the base name and every numbered suffix NextBackupPath will try,
	// so the bound is what stops the search, not luck finding a free name.
	if err := os.WriteFile(base, nil, 0o644); err != nil {
		t.Fatalf("seed base: %v", err)
	}
	for n := 2; n <= maxBackupNameAttempts; n++ {
		if err := os.WriteFile(fmt.Sprintf("%s.%d", base, n), nil, 0o644); err != nil {
			t.Fatalf("seed suffix .%d: %v", n, err)
		}
	}

	if _, err := s.NextBackupPath(at); err == nil {
		t.Fatalf("NextBackupPath: want an error once every name up to the bound is taken, got nil")
	}
}

// ---------------------------------------------------------------------
// DedupeHistory / ApplyDedupeHistory: full DB round trip
// ---------------------------------------------------------------------

func openRepairTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	s, err := Open(context.Background())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.DB.Close() })
	return s, dir
}

// seedRepairFixture writes two sessions:
//   - "sess-gone": backing JSONL path recorded but the file doesn't exist.
//     Turns 0-2 are one duplicated API response (same tuple, ms apart);
//     turn 3 is a real, later, differently-priced turn. Also carries one
//     compaction and one user prompt, which the repair must never touch.
//   - "sess-present": backing JSONL still exists on disk, so it must be
//     excluded from the default (non---all) scope even though its two
//     turns would otherwise collapse just like sess-gone's.
func seedRepairFixture(t *testing.T, s *Store, dir string) {
	t.Helper()
	ctx := context.Background()

	goneHash := "hash-gone"
	gap := 12.5
	if err := s.ReplaceSessionData(ctx, SessionPersist{
		SessionUUID: "sess-gone",
		Project:     "proj",
		Turns: []TurnRow{
			{SessionUUID: "sess-gone", TurnIdx: 0, TS: "t0", TSUnixMS: 1_000, Model: "m",
				InputTokens: 10, OutputTokens: 5, GapS: 42, Classification: "normal",
				PostCompact: false, Project: "proj", SourcePathHash: goneHash},
			{SessionUUID: "sess-gone", TurnIdx: 1, TS: "t1", TSUnixMS: 1_010, Model: "m",
				InputTokens: 10, OutputTokens: 5, GapS: 0.01, Classification: "rotation",
				PostCompact: true, Project: "proj", SourcePathHash: goneHash},
			{SessionUUID: "sess-gone", TurnIdx: 2, TS: "t2", TSUnixMS: 1_020, Model: "m",
				InputTokens: 10, OutputTokens: 5, GapS: 0.01, Classification: "rotation",
				PostCompact: false, Project: "proj", SourcePathHash: goneHash},
			{SessionUUID: "sess-gone", TurnIdx: 3, TS: "t3", TSUnixMS: 500_000, Model: "m2",
				InputTokens: 99, OutputTokens: 1, GapS: 5, Classification: "normal",
				PostCompact: false, Project: "proj", SourcePathHash: goneHash},
		},
		Compactions: []CompactionRow{{
			SessionUUID: "sess-gone", TS: "tc", TSUnixMS: 999,
			PrefixTokensEst: 10, SummaryTokensEst: 2, GapToPrevS: &gap,
			CacheState: "cold", Confirmed: true, Project: "proj",
		}},
		UserPrompts: []UserPromptRow{{SessionUUID: "sess-gone", TSUnixMS: 500, TextPreview: "hi"}},
	}); err != nil {
		t.Fatalf("seed sess-gone: %v", err)
	}
	if err := s.RecordIngestedFile(ctx, IngestedFileRecord{
		PathHash: goneHash, Path: filepath.Join(dir, "deleted-transcript.jsonl"),
		MTimeUnix: 1, TurnCount: 4,
	}); err != nil {
		t.Fatalf("record ingested_files (gone): %v", err)
	}

	presentPath := filepath.Join(dir, "present.jsonl")
	if err := os.WriteFile(presentPath, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write present.jsonl: %v", err)
	}
	presentHash := "hash-present"
	if err := s.ReplaceSessionData(ctx, SessionPersist{
		SessionUUID: "sess-present",
		Project:     "proj",
		Turns: []TurnRow{
			{SessionUUID: "sess-present", TurnIdx: 0, TS: "p0", TSUnixMS: 1_000, Model: "m",
				InputTokens: 1, OutputTokens: 1, GapS: 7, Classification: "normal",
				Project: "proj", SourcePathHash: presentHash},
			{SessionUUID: "sess-present", TurnIdx: 1, TS: "p1", TSUnixMS: 1_005, Model: "m",
				InputTokens: 1, OutputTokens: 1, GapS: 0.01, Classification: "normal",
				Project: "proj", SourcePathHash: presentHash},
		},
	}); err != nil {
		t.Fatalf("seed sess-present: %v", err)
	}
	if err := s.RecordIngestedFile(ctx, IngestedFileRecord{
		PathHash: presentHash, Path: presentPath, MTimeUnix: 1, TurnCount: 2,
	}); err != nil {
		t.Fatalf("record ingested_files (present): %v", err)
	}
}

func TestDedupeHistoryDefaultScopeExcludesSessionsStillOnDisk(t *testing.T) {
	s, dir := openRepairTestStore(t)
	seedRepairFixture(t, s, dir)
	ctx := context.Background()

	stats, err := s.DedupeHistory(ctx, RepairOptions{WindowS: 60})
	if err != nil {
		t.Fatalf("DedupeHistory: %v", err)
	}
	if stats.SessionsInScope != 1 {
		t.Fatalf("SessionsInScope = %d, want 1 (only sess-gone; sess-present's file exists)", stats.SessionsInScope)
	}
	if stats.RunsCollapsed != 1 || stats.TurnsRemoved != 2 {
		t.Fatalf("got RunsCollapsed=%d TurnsRemoved=%d, want 1 and 2 (turns 0-2 -> 1 row)",
			stats.RunsCollapsed, stats.TurnsRemoved)
	}

	allStats, err := s.DedupeHistory(ctx, RepairOptions{WindowS: 60, All: true})
	if err != nil {
		t.Fatalf("DedupeHistory --all: %v", err)
	}
	if allStats.SessionsInScope != 2 {
		t.Fatalf("--all SessionsInScope = %d, want 2", allStats.SessionsInScope)
	}
	// sess-present's two turns are 5ms apart with an identical tuple, so
	// --all must find a second collapsible run.
	if allStats.RunsCollapsed != 2 {
		t.Fatalf("--all RunsCollapsed = %d, want 2 (sess-gone's run + sess-present's)", allStats.RunsCollapsed)
	}
}

func TestDedupeHistoryDryRunWritesNothing(t *testing.T) {
	s, dir := openRepairTestStore(t)
	seedRepairFixture(t, s, dir)
	ctx := context.Background()

	if _, err := s.DedupeHistory(ctx, RepairOptions{WindowS: 60}); err != nil {
		t.Fatalf("DedupeHistory: %v", err)
	}

	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM turns WHERE session_uuid = 'sess-gone'`).Scan(&n); err != nil {
		t.Fatalf("count turns: %v", err)
	}
	if n != 4 {
		t.Fatalf("turns for sess-gone = %d, want 4 (dry run must not write)", n)
	}
}

func TestApplyDedupeHistoryCollapsesRenamesPreservesFieldsAndOtherTables(t *testing.T) {
	s, dir := openRepairTestStore(t)
	seedRepairFixture(t, s, dir)
	ctx := context.Background()

	stats, err := s.ApplyDedupeHistory(ctx, RepairOptions{WindowS: 60})
	if err != nil {
		t.Fatalf("ApplyDedupeHistory: %v", err)
	}
	if stats.RunsCollapsed != 1 || stats.TurnsRemoved != 2 || stats.SessionsChanged != 1 {
		t.Fatalf("stats = %+v, want RunsCollapsed=1 TurnsRemoved=2 SessionsChanged=1", stats)
	}

	rows, err := s.DB.Query(`
		SELECT turn_idx, ts_unix_ms, model, input_tokens, gap_s, classification, post_compact
		FROM turns WHERE session_uuid = 'sess-gone' ORDER BY turn_idx`)
	if err != nil {
		t.Fatalf("query turns: %v", err)
	}
	defer rows.Close()

	type got struct {
		idx         int
		tsMS        int64
		model       string
		in          int
		gap         float64
		class       string
		postCompact int
	}
	var out []got
	for rows.Next() {
		var g got
		if err := rows.Scan(&g.idx, &g.tsMS, &g.model, &g.in, &g.gap, &g.class, &g.postCompact); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, g)
	}
	if len(out) != 2 {
		t.Fatalf("post-apply turn count = %d, want 2", len(out))
	}

	// Row 0: the collapsed run. ts/ts_unix_ms from the LAST duplicate
	// (turn_idx 2, ts_unix_ms 1020); gap_s/classification from the FIRST
	// (turn_idx 0: gap_s 42, classification "normal"); post_compact OR'd
	// across the run (turn_idx 1 had it set).
	if out[0].idx != 0 {
		t.Errorf("row0 turn_idx = %d, want 0", out[0].idx)
	}
	if out[0].tsMS != 1_020 {
		t.Errorf("row0 ts_unix_ms = %d, want 1020 (last row's)", out[0].tsMS)
	}
	if out[0].gap != 42 {
		t.Errorf("row0 gap_s = %v, want 42 (first row's)", out[0].gap)
	}
	if out[0].class != "normal" {
		t.Errorf("row0 classification = %q, want %q (first row's)", out[0].class, "normal")
	}
	if out[0].postCompact != 1 {
		t.Errorf("row0 post_compact = %d, want 1 (OR'd across the run)", out[0].postCompact)
	}

	// Row 1: the untouched 4th turn, renumbered from 3 to 1.
	if out[1].idx != 1 {
		t.Errorf("row1 turn_idx = %d, want 1 (renumbered dense)", out[1].idx)
	}
	if out[1].model != "m2" || out[1].in != 99 {
		t.Errorf("row1 = %+v, want the untouched model=m2 input=99 turn", out[1])
	}

	// sess-present must be completely untouched: still on disk, so out of
	// the default scope.
	var presentCount int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM turns WHERE session_uuid = 'sess-present'`).Scan(&presentCount); err != nil {
		t.Fatalf("count sess-present turns: %v", err)
	}
	if presentCount != 2 {
		t.Fatalf("sess-present turn count = %d, want 2 (untouched)", presentCount)
	}

	// Compactions and user_prompts must survive: repair only ever touches turns.
	var compactCount, promptCount int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM compactions WHERE session_uuid = 'sess-gone'`).Scan(&compactCount); err != nil {
		t.Fatalf("count compactions: %v", err)
	}
	if compactCount != 1 {
		t.Fatalf("compactions for sess-gone = %d, want 1 (must survive untouched)", compactCount)
	}
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM user_prompts WHERE session_uuid = 'sess-gone'`).Scan(&promptCount); err != nil {
		t.Fatalf("count user_prompts: %v", err)
	}
	if promptCount != 1 {
		t.Fatalf("user_prompts for sess-gone = %d, want 1 (must survive untouched)", promptCount)
	}
}

func TestApplyDedupeHistoryIsIdempotent(t *testing.T) {
	s, dir := openRepairTestStore(t)
	seedRepairFixture(t, s, dir)
	ctx := context.Background()
	opts := RepairOptions{WindowS: 60}

	if _, err := s.ApplyDedupeHistory(ctx, opts); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	second, err := s.ApplyDedupeHistory(ctx, opts)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if second.RunsCollapsed != 0 || second.TurnsRemoved != 0 || second.SessionsChanged != 0 {
		t.Fatalf("second apply changed things: %+v, want an all-zero no-op", second)
	}
}

func TestApplyDedupeHistoryLeavesTurnIdxDenseAndGapFree(t *testing.T) {
	s, dir := openRepairTestStore(t)
	seedRepairFixture(t, s, dir)
	ctx := context.Background()

	if _, err := s.ApplyDedupeHistory(ctx, RepairOptions{WindowS: 60, All: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// Mirrors the exact query from the repair verification checklist.
	rows, err := s.DB.Query(`
		SELECT session_uuid, COUNT(*), MIN(turn_idx), MAX(turn_idx)
		FROM turns GROUP BY session_uuid
		HAVING MAX(turn_idx) != COUNT(*) - 1 OR MIN(turn_idx) != 0`)
	if err != nil {
		t.Fatalf("density query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var uuid string
		var cnt, lo, hi int
		_ = rows.Scan(&uuid, &cnt, &lo, &hi)
		t.Errorf("session %s has non-dense turn_idx: count=%d min=%d max=%d", uuid, cnt, lo, hi)
	}
}
