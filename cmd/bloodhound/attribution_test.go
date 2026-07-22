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
