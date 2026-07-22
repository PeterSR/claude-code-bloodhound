package server

import (
	"testing"

	"github.com/PeterSR/claude-code-bloodhound/internal/attribute"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

// TestBuildAttrWindows_BySessionFoldsSubagentIntoParent is the Regression 2
// fix: a subagent's slice must land in the same bucket as its parent's, the
// same effective-owner rule GroupAttribution already applies to the table
// beneath this chart. Before this, a window's slices summed to a different
// total-per-key than the group table below it for the same window.
func TestBuildAttrWindows_BySessionFoldsSubagentIntoParent(t *testing.T) {
	windows := []store.LimitWindowRow{
		{StartUnixMS: 1000, EndUnixMS: 2000, AttributedPct: 25},
	}
	slices := []store.AttributionRow{
		{
			WindowStartUnixMS: 1000, SessionUUID: "parent", EffectiveSessionUUID: "parent",
			Project: "proj", MeasuredPct: 5,
		},
		{
			// A subagent's own row: keyed by its own uuid in session_attribution,
			// but its EffectiveSessionUUID (as WindowSlices now computes it)
			// points back at the parent.
			WindowStartUnixMS: 1000, SessionUUID: "agent-sub1", EffectiveSessionUUID: "parent",
			Project: "proj", MeasuredPct: 20,
		},
	}

	out := buildAttrWindows(windows, slices, "session")
	if len(out) != 1 {
		t.Fatalf("got %d windows, want 1", len(out))
	}
	got := out[0].Slices
	if len(got) != 1 {
		t.Fatalf("got %d slices, want 1 (parent + subagent folded together): %+v", len(got), got)
	}
	if got[0].Key != "parent" {
		t.Errorf("slice key = %q, want %q", got[0].Key, "parent")
	}
	if got[0].Pct != 25 {
		t.Errorf("slice pct = %v, want 25 (parent's 5 plus subagent's 20)", got[0].Pct)
	}

	// The point of the fix: this must equal the window's attributed_pct, the
	// same number GroupAttribution's table sums to for this window.
	if got[0].Pct != out[0].AttributedPct {
		t.Errorf("slice pct %v does not equal window attributed_pct %v; chart and table would disagree", got[0].Pct, out[0].AttributedPct)
	}

	// No slice key may be the subagent's own uuid: it must never show up as
	// its parent's peer.
	for _, sl := range got {
		if sl.Key == "agent-sub1" {
			t.Errorf("subagent uuid %q appeared as its own slice key, want it folded into the parent", sl.Key)
		}
	}
}

// TestBuildAttrWindows_ByProjectUnaffected guards the other half of the
// comment on buildAttrWindows: grouping by project must keep summing
// parent + subagent rows into one project slice exactly as before, since a
// subagent already inherits its parent's project (see internal/ingest).
// EffectiveSessionUUID is irrelevant in this mode.
func TestBuildAttrWindows_ByProjectUnaffected(t *testing.T) {
	windows := []store.LimitWindowRow{{StartUnixMS: 1000, EndUnixMS: 2000, AttributedPct: 25}}
	slices := []store.AttributionRow{
		{WindowStartUnixMS: 1000, SessionUUID: "parent", EffectiveSessionUUID: "parent", Project: "proj", MeasuredPct: 5},
		{WindowStartUnixMS: 1000, SessionUUID: "agent-sub1", EffectiveSessionUUID: "parent", Project: "proj", MeasuredPct: 20},
	}

	out := buildAttrWindows(windows, slices, "project")
	if len(out) != 1 || len(out[0].Slices) != 1 {
		t.Fatalf("got %+v, want a single 'proj' slice", out)
	}
	if out[0].Slices[0].Key != "proj" || out[0].Slices[0].Pct != 25 {
		t.Errorf("got slice %+v, want key=proj pct=25", out[0].Slices[0])
	}
}

// TestBuildAttrWindows_UnattributedStaysItsOwnBucket makes sure the
// EffectiveSessionUUID change didn't disturb the unattributed sentinel: an
// empty session_uuid must still land in its own hatched bucket, in both
// groupings, rather than being folded anywhere by the LEFT JOIN having no
// match for it.
func TestBuildAttrWindows_UnattributedStaysItsOwnBucket(t *testing.T) {
	windows := []store.LimitWindowRow{{StartUnixMS: 1000, EndUnixMS: 2000, AttributedPct: 10}}
	slices := []store.AttributionRow{
		{WindowStartUnixMS: 1000, SessionUUID: attribute.Unattributed, EffectiveSessionUUID: attribute.Unattributed, Project: "", MeasuredPct: 10},
	}

	out := buildAttrWindows(windows, slices, "session")
	if len(out[0].Slices) != 1 || out[0].Slices[0].Key != attribute.Unattributed {
		t.Fatalf("got %+v, want a single unattributed slice", out[0].Slices)
	}
}
