package main

import (
	"strings"
	"testing"

	"github.com/PeterSR/claude-code-bloodhound/internal/attribute"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

// TestFoldSlicesByEffectiveOwner_SubagentFoldsIntoParent is the CLI-side
// half of the fix in 503eb06 (which covered the web stacked chart via
// buildAttrWindows): `attribution windows --slices` must key on
// EffectiveSessionUUID the same way GroupAttribution does, or the two
// commands attribute the same spend to different keys.
func TestFoldSlicesByEffectiveOwner_SubagentFoldsIntoParent(t *testing.T) {
	rows := []store.AttributionRow{
		{
			SessionUUID: "parent", EffectiveSessionUUID: "parent", Project: "proj",
			MeasuredPct: 5, TurnCount: 3, CWTokens: 100, RawTokens: 1000,
			FirstTSUnixMS: 2000, LastTSUnixMS: 3000,
		},
		{
			// A subagent's own row: keyed by its own uuid in
			// session_attribution, but WindowSlices resolves its
			// EffectiveSessionUUID back to the session that dispatched it.
			SessionUUID: "agent-sub1", EffectiveSessionUUID: "parent", Project: "proj",
			MeasuredPct: 20, TurnCount: 7, CWTokens: 400, RawTokens: 4000,
			FirstTSUnixMS: 1000, LastTSUnixMS: 4000,
		},
		{
			SessionUUID: "other", EffectiveSessionUUID: "other", Project: "proj",
			MeasuredPct: 1, TurnCount: 1, CWTokens: 10, RawTokens: 100,
		},
	}

	out := foldSlicesByEffectiveOwner(rows)
	if len(out) != 2 {
		t.Fatalf("got %d slices, want 2 (parent+subagent folded together, plus other): %+v", len(out), out)
	}

	// Largest share first: the folded parent (25) must outrank "other" (1),
	// even though the raw parent row (5) alone would not have.
	if out[0].SessionUUID != "parent" {
		t.Fatalf("out[0].SessionUUID = %q, want %q (largest combined share first)", out[0].SessionUUID, "parent")
	}
	parent := out[0]
	if got, want := parent.MeasuredPct+parent.EstimatedPct, 25.0; got != want {
		t.Errorf("parent pct = %v, want %v (5 + 20)", got, want)
	}
	if got, want := parent.TurnCount, 10; got != want {
		t.Errorf("parent TurnCount = %d, want %d (3 + 7)", got, want)
	}
	if got, want := parent.CWTokens, 500.0; got != want {
		t.Errorf("parent CWTokens = %v, want %v (100 + 400)", got, want)
	}
	if got, want := parent.RawTokens, int64(5000); got != want {
		t.Errorf("parent RawTokens = %d, want %d (1000 + 4000)", got, want)
	}
	if got, want := parent.FirstTSUnixMS, int64(1000); got != want {
		t.Errorf("parent FirstTSUnixMS = %d, want %d (earliest of the two)", got, want)
	}
	if got, want := parent.LastTSUnixMS, int64(4000); got != want {
		t.Errorf("parent LastTSUnixMS = %d, want %d (latest of the two)", got, want)
	}

	// No slice key may be the subagent's own uuid: it must never show up as
	// its parent's peer.
	for _, r := range out {
		if r.SessionUUID == "agent-sub1" {
			t.Errorf("subagent uuid %q appeared as its own slice key, want it folded into the parent", r.SessionUUID)
		}
	}
}

// TestFoldSlicesByEffectiveOwner_UnattributedStaysItsOwnBucket guards the
// unattributed sentinel: an empty session_uuid must still land in its own
// bucket rather than being folded into a real session by an unlucky
// LEFT JOIN match.
func TestFoldSlicesByEffectiveOwner_UnattributedStaysItsOwnBucket(t *testing.T) {
	rows := []store.AttributionRow{
		{SessionUUID: attribute.Unattributed, EffectiveSessionUUID: attribute.Unattributed, MeasuredPct: 10},
		{SessionUUID: "real", EffectiveSessionUUID: "real", MeasuredPct: 5},
	}
	out := foldSlicesByEffectiveOwner(rows)
	if len(out) != 2 {
		t.Fatalf("got %d slices, want 2: %+v", len(out), out)
	}
	found := false
	for _, r := range out {
		if r.SessionUUID == attribute.Unattributed {
			found = true
			if r.MeasuredPct != 10 {
				t.Errorf("unattributed pct = %v, want 10", r.MeasuredPct)
			}
		}
	}
	if !found {
		t.Fatalf("no unattributed bucket in output: %+v", out)
	}
}

// TestFoldSlicesByEffectiveOwner_NoSubagentsUnaffected makes sure the fold
// is a no-op (aside from the sort) when nothing shares an effective owner,
// so the common case (no subagents in this window) isn't disturbed.
func TestFoldSlicesByEffectiveOwner_NoSubagentsUnaffected(t *testing.T) {
	rows := []store.AttributionRow{
		{SessionUUID: "a", EffectiveSessionUUID: "a", MeasuredPct: 1, TurnCount: 1},
		{SessionUUID: "b", EffectiveSessionUUID: "b", MeasuredPct: 9, TurnCount: 2},
	}
	out := foldSlicesByEffectiveOwner(rows)
	if len(out) != 2 {
		t.Fatalf("got %d slices, want 2: %+v", len(out), out)
	}
	if out[0].SessionUUID != "b" || out[0].MeasuredPct != 9 {
		t.Errorf("out[0] = %+v, want b/9 first (largest share)", out[0])
	}
}

// TestAttrResolveBy_AcceptsCwd is the CLI-side guard for the third grouping:
// --by cwd must resolve like "project" and "session" already do, and
// anything else must still be rejected with a message naming all three.
func TestAttrResolveBy_AcceptsCwd(t *testing.T) {
	orig := attrBy
	defer func() { attrBy = orig }()

	for _, valid := range []string{"project", "session", "cwd"} {
		attrBy = valid
		got, err := attrResolveBy()
		if err != nil {
			t.Errorf("attrResolveBy() with --by %q: unexpected error %v", valid, err)
		}
		if got != valid {
			t.Errorf("attrResolveBy() with --by %q = %q, want %q", valid, got, valid)
		}
	}

	attrBy = "bogus"
	if _, err := attrResolveBy(); err == nil {
		t.Fatalf("attrResolveBy() with --by \"bogus\": want an error")
	} else if !strings.Contains(err.Error(), "cwd") {
		t.Errorf("attrResolveBy() error %q, want it to mention \"cwd\" as a valid option", err.Error())
	}
}

// TestAttrResolveWindow_AcceptsCurrentOnly is the CLI-side guard for
// --window: "" and "current" resolve, anything else is rejected rather than
// silently ignored (unlike the API's equally permissive query param of the
// same name), since a CLI typo deserves a message.
func TestAttrResolveWindow_AcceptsCurrentOnly(t *testing.T) {
	orig := attrWindow
	defer func() { attrWindow = orig }()

	for _, valid := range []string{"", "current"} {
		attrWindow = valid
		got, err := attrResolveWindow()
		if err != nil {
			t.Errorf("attrResolveWindow() with --window %q: unexpected error %v", valid, err)
		}
		if got != valid {
			t.Errorf("attrResolveWindow() with --window %q = %q, want %q", valid, got, valid)
		}
	}

	attrWindow = "bogus"
	if _, err := attrResolveWindow(); err == nil {
		t.Fatalf("attrResolveWindow() with --window \"bogus\": want an error")
	} else if !strings.Contains(err.Error(), "current") {
		t.Errorf("attrResolveWindow() error %q, want it to mention \"current\"", err.Error())
	}
}

// TestAttrCurrentWindow finds the one in-progress window in a bucket's
// list, or reports none: the same lookup --window current relies on to
// scope the rollup to a single window.
func TestAttrCurrentWindow(t *testing.T) {
	wins := []store.LimitWindowRow{
		{StartUnixMS: 1000, InProgress: false},
		{StartUnixMS: 2000, InProgress: true},
	}
	got := attrCurrentWindow(wins)
	if got == nil || got.StartUnixMS != 2000 {
		t.Fatalf("attrCurrentWindow = %+v, want the window starting at 2000", got)
	}

	none := []store.LimitWindowRow{{StartUnixMS: 1000, InProgress: false}}
	if got := attrCurrentWindow(none); got != nil {
		t.Errorf("attrCurrentWindow with no open window = %+v, want nil", got)
	}
}

// TestBuildAttrGroupWindows_FoldsSubagentIntoParent is the CLI-side twin of
// the server package's test of the same shape: --per-window must fold a
// subagent's slice into its parent's array, the same effective-owner rule
// the rest of the rollup already applies, or --per-window would disagree
// with the plain rollup about where a window's spend went.
func TestBuildAttrGroupWindows_FoldsSubagentIntoParent(t *testing.T) {
	windows := []store.LimitWindowRow{
		{StartUnixMS: 1000, EndUnixMS: 2000, InProgress: false},
		{StartUnixMS: 2000, EndUnixMS: 3000, InProgress: true},
	}
	slices := []store.AttributionRow{
		{WindowStartUnixMS: 1000, SessionUUID: "parent", EffectiveSessionUUID: "parent", MeasuredPct: 5, TurnCount: 2},
		{WindowStartUnixMS: 1000, SessionUUID: "agent-sub1", EffectiveSessionUUID: "parent", MeasuredPct: 20, TurnCount: 7},
		{WindowStartUnixMS: 2000, SessionUUID: "parent", EffectiveSessionUUID: "parent", MeasuredPct: 8, TurnCount: 1},
	}

	out := buildAttrGroupWindows(windows, slices, "session")
	pw, ok := out["parent"]
	if !ok || len(pw) != 2 {
		t.Fatalf("got %+v, want 2 window entries under \"parent\"", out)
	}
	if pw[0].WindowStartUnixMS != 1000 || pw[0].Pct != 25 {
		t.Errorf("pw[0] = %+v, want window 1000 with pct 25 (5 + 20)", pw[0])
	}
	if pw[1].WindowStartUnixMS != 2000 || pw[1].Pct != 8 || !pw[1].InProgress {
		t.Errorf("pw[1] = %+v, want window 2000, pct 8, in progress", pw[1])
	}
	if _, ok := out["agent-sub1"]; ok {
		t.Errorf("subagent must not have its own key in the per-group map: %+v", out)
	}
}

// TestRollupGroupKey_MatchesEachByMode checks the key derivation
// buildAttrGroupWindows depends on for all three groupings plus both
// sentinels, since a wrong key here would silently orphan a --per-window
// array from its group (map lookups don't fail loudly on a mismatched key).
func TestRollupGroupKey_MatchesEachByMode(t *testing.T) {
	cases := []struct {
		name string
		row  store.AttributionRow
		by   string
		want string
	}{
		{"project", store.AttributionRow{SessionUUID: "sess", Project: "proj", EffectiveSessionUUID: "sess", Cwd: "/x"}, "project", "proj"},
		{"session", store.AttributionRow{SessionUUID: "sess", Project: "proj", EffectiveSessionUUID: "sess", Cwd: "/x"}, "session", "sess"},
		{"cwd", store.AttributionRow{SessionUUID: "sess", Project: "proj", EffectiveSessionUUID: "sess", Cwd: "/x"}, "cwd", "/x"},
		{"cwd unknown", store.AttributionRow{SessionUUID: "known", EffectiveSessionUUID: "known", Cwd: ""}, "cwd", store.UnknownCwd},
		{"unattributed session", store.AttributionRow{SessionUUID: attribute.Unattributed, EffectiveSessionUUID: attribute.Unattributed}, "session", attribute.Unattributed},
		{"unattributed cwd", store.AttributionRow{SessionUUID: attribute.Unattributed, EffectiveSessionUUID: attribute.Unattributed}, "cwd", attribute.Unattributed},
	}
	for _, c := range cases {
		if got := rollupGroupKey(c.row, c.by); got != c.want {
			t.Errorf("%s: rollupGroupKey(%+v, %q) = %q, want %q", c.name, c.row, c.by, got, c.want)
		}
	}
}

// TestRollupLastColumn_CwdUnknownBucketIsLabeled makes sure the human-mode
// rollup renders store.UnknownCwd as a readable label, distinct from the
// unattributed sentinel's own "(unattributed)" and from a real directory,
// so a reader scanning the last column can tell all three apart at a
// glance.
func TestRollupLastColumn_CwdUnknownBucketIsLabeled(t *testing.T) {
	unknown := store.AttributionGroup{Key: store.UnknownCwd}
	if got, want := rollupLastColumn(unknown, "cwd"), "(unknown cwd)"; got != want {
		t.Errorf("rollupLastColumn(UnknownCwd, \"cwd\") = %q, want %q", got, want)
	}

	unattributed := store.AttributionGroup{Key: attribute.Unattributed}
	if got, want := rollupLastColumn(unattributed, "cwd"), "(unattributed)"; got != want {
		t.Errorf("rollupLastColumn(unattributed, \"cwd\") = %q, want %q", got, want)
	}

	real := store.AttributionGroup{Key: "/home/user/projects/myapp"}
	if got, want := rollupLastColumn(real, "cwd"), "/home/user/projects/myapp"; got != want {
		t.Errorf("rollupLastColumn(real cwd, \"cwd\") = %q, want %q", got, want)
	}
}
