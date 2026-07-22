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

// TestBuildAttrWindows_ByCwdFoldsSubagentIntoParent is the by=="cwd"
// counterpart of TestBuildAttrWindows_BySessionFoldsSubagentIntoParent: a
// subagent's slice must land under its dispatcher's directory (WindowSlices
// already resolves Cwd to the effective owner's, never the subagent's own),
// so the chart agrees with GroupAttribution's by=="cwd" table beneath it.
func TestBuildAttrWindows_ByCwdFoldsSubagentIntoParent(t *testing.T) {
	windows := []store.LimitWindowRow{{StartUnixMS: 1000, EndUnixMS: 2000, AttributedPct: 25}}
	slices := []store.AttributionRow{
		{
			WindowStartUnixMS: 1000, SessionUUID: "parent", EffectiveSessionUUID: "parent",
			Project: "proj", Cwd: "/home/user/projects/myapp", MeasuredPct: 5,
		},
		{
			// A subagent's own row: WindowSlices resolves its Cwd to the
			// PARENT's directory, never wherever the subagent itself ran.
			WindowStartUnixMS: 1000, SessionUUID: "agent-sub1", EffectiveSessionUUID: "parent",
			Project: "proj", Cwd: "/home/user/projects/myapp", MeasuredPct: 20,
		},
	}

	out := buildAttrWindows(windows, slices, "cwd")
	if len(out) != 1 {
		t.Fatalf("got %d windows, want 1", len(out))
	}
	got := out[0].Slices
	if len(got) != 1 {
		t.Fatalf("got %d slices, want 1 (parent + subagent folded together): %+v", len(got), got)
	}
	if got[0].Key != "/home/user/projects/myapp" {
		t.Errorf("slice key = %q, want the shared directory", got[0].Key)
	}
	if got[0].Pct != 25 {
		t.Errorf("slice pct = %v, want 25 (parent's 5 plus subagent's 20)", got[0].Pct)
	}
}

// TestBuildAttrGroupWindows_BySessionFoldsSubagentIntoParent is
// buildAttrWindows's fold test transposed: buildAttrGroupWindows must fold a
// subagent's slice into its parent's per-window array the same way
// buildAttrWindows already folds it into the parent's per-window slice, so
// --per-window can't disagree with the ordinary rollup (or the stacked
// chart) about where a window's spend went.
func TestBuildAttrGroupWindows_BySessionFoldsSubagentIntoParent(t *testing.T) {
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
	if !ok {
		t.Fatalf("no per-window array for %q: %+v", "parent", out)
	}
	if len(pw) != 2 {
		t.Fatalf("got %d window entries, want 2 (one per window, subagent folded rather than exploded): %+v", len(pw), pw)
	}
	if got, want := pw[0].WindowStartUnixMS, int64(1000); got != want {
		t.Fatalf("pw[0].WindowStartUnixMS = %d, want %d (oldest first)", got, want)
	}
	if got, want := pw[0].Pct, 25.0; got != want {
		t.Errorf("window 1 pct = %v, want %v (parent's 5 plus subagent's 20)", got, want)
	}
	if got := pw[0].InProgress; got {
		t.Errorf("window 1 InProgress = %v, want false", got)
	}
	if got, want := pw[1].Pct, 8.0; got != want {
		t.Errorf("window 2 pct = %v, want %v (parent alone)", got, want)
	}
	if got := pw[1].InProgress; !got {
		t.Errorf("window 2 InProgress = %v, want true", got)
	}

	if _, ok := out["agent-sub1"]; ok {
		t.Errorf("subagent %q must not have its own per-window array; its cost belongs under the parent's key", "agent-sub1")
	}

	// The whole point: sum(PerWindow) must equal the same window's slice in
	// buildAttrWindows, so the two views of the same data can't disagree.
	byWin := buildAttrWindows(windows, slices, "session")
	var sum float64
	for _, e := range pw {
		sum += e.Pct
	}
	var wantSum float64
	for _, w := range byWin {
		for _, sl := range w.Slices {
			if sl.Key == "parent" {
				wantSum += sl.Pct
			}
		}
	}
	if sum != wantSum {
		t.Errorf("sum of PerWindow = %v, want %v (buildAttrWindows' parent slices summed)", sum, wantSum)
	}
}

// TestBuildAttrGroupWindows_ByProjectUnaffected mirrors
// TestBuildAttrWindows_ByProjectUnaffected: a subagent already inherits its
// parent's project, so by=="project" pools them without any fold logic.
func TestBuildAttrGroupWindows_ByProjectUnaffected(t *testing.T) {
	windows := []store.LimitWindowRow{{StartUnixMS: 1000, EndUnixMS: 2000}}
	slices := []store.AttributionRow{
		{WindowStartUnixMS: 1000, SessionUUID: "parent", EffectiveSessionUUID: "parent", Project: "proj", MeasuredPct: 5},
		{WindowStartUnixMS: 1000, SessionUUID: "agent-sub1", EffectiveSessionUUID: "parent", Project: "proj", MeasuredPct: 20},
	}
	out := buildAttrGroupWindows(windows, slices, "project")
	pw, ok := out["proj"]
	if !ok || len(pw) != 1 || pw[0].Pct != 25 {
		t.Fatalf("got %+v, want a single 'proj' window entry with pct 25", out)
	}
}

// TestBuildAttrGroupWindows_UnattributedAndUnknownCwdStayDistinct guards the
// two sentinels the same way TestBuildAttrWindows_ByCwdUnknownGetsOwnBucket
// does for buildAttrWindows: they must not collide, and neither must be
// dropped, when transposed into the per-group view.
func TestBuildAttrGroupWindows_UnattributedAndUnknownCwdStayDistinct(t *testing.T) {
	windows := []store.LimitWindowRow{{StartUnixMS: 1000, EndUnixMS: 2000}}
	slices := []store.AttributionRow{
		{WindowStartUnixMS: 1000, SessionUUID: "known", EffectiveSessionUUID: "known", Cwd: "", MeasuredPct: 7},
		{WindowStartUnixMS: 1000, SessionUUID: attribute.Unattributed, EffectiveSessionUUID: attribute.Unattributed, Cwd: "", MeasuredPct: 3},
	}
	out := buildAttrGroupWindows(windows, slices, "cwd")
	if len(out) != 2 {
		t.Fatalf("got %d keys, want 2 (UnknownCwd and unattributed kept apart): %+v", len(out), out)
	}
	if pw, ok := out[store.UnknownCwd]; !ok || len(pw) != 1 || pw[0].Pct != 7 {
		t.Errorf("UnknownCwd entry = %+v, want a single window with pct 7", pw)
	}
	if pw, ok := out[attribute.Unattributed]; !ok || len(pw) != 1 || pw[0].Pct != 3 {
		t.Errorf("unattributed entry = %+v, want a single window with pct 3", pw)
	}
}

// TestCurrentLimitWindow finds the one in-progress window, or reports none.
func TestCurrentLimitWindow(t *testing.T) {
	windows := []store.LimitWindowRow{
		{StartUnixMS: 1000, InProgress: false},
		{StartUnixMS: 2000, InProgress: false},
		{StartUnixMS: 3000, InProgress: true},
	}
	got := currentLimitWindow(windows)
	if got == nil || got.StartUnixMS != 3000 {
		t.Fatalf("currentLimitWindow = %+v, want the window starting at 3000", got)
	}

	none := []store.LimitWindowRow{{StartUnixMS: 1000, InProgress: false}}
	if got := currentLimitWindow(none); got != nil {
		t.Errorf("currentLimitWindow with no open window = %+v, want nil", got)
	}
	if got := currentLimitWindow(nil); got != nil {
		t.Errorf("currentLimitWindow(nil) = %+v, want nil", got)
	}
}

// TestBuildAttrWindows_ByCwdUnknownGetsOwnBucket covers the other new case:
// a real, known session whose cwd was never captured must key on
// store.UnknownCwd, not on the empty string (which would either vanish or
// collide with a genuinely unattributed slice sharing the same window).
func TestBuildAttrWindows_ByCwdUnknownGetsOwnBucket(t *testing.T) {
	windows := []store.LimitWindowRow{{StartUnixMS: 1000, EndUnixMS: 2000, AttributedPct: 10}}
	slices := []store.AttributionRow{
		{WindowStartUnixMS: 1000, SessionUUID: "known", EffectiveSessionUUID: "known", Cwd: "", MeasuredPct: 7},
		{WindowStartUnixMS: 1000, SessionUUID: attribute.Unattributed, EffectiveSessionUUID: attribute.Unattributed, Cwd: "", MeasuredPct: 3},
	}

	out := buildAttrWindows(windows, slices, "cwd")
	if len(out[0].Slices) != 2 {
		t.Fatalf("got %d slices, want 2 (UnknownCwd and unattributed kept apart): %+v", len(out[0].Slices), out[0].Slices)
	}

	var unknownPct, unattributedPct float64
	var sawUnknown, sawUnattributed bool
	for _, sl := range out[0].Slices {
		switch sl.Key {
		case store.UnknownCwd:
			sawUnknown, unknownPct = true, sl.Pct
		case attribute.Unattributed:
			sawUnattributed, unattributedPct = true, sl.Pct
		}
	}
	if !sawUnknown {
		t.Fatalf("no store.UnknownCwd slice: %+v", out[0].Slices)
	}
	if !sawUnattributed {
		t.Fatalf("no unattributed slice: %+v", out[0].Slices)
	}
	if unknownPct != 7 {
		t.Errorf("UnknownCwd slice pct = %v, want 7", unknownPct)
	}
	if unattributedPct != 3 {
		t.Errorf("unattributed slice pct = %v, want 3", unattributedPct)
	}
}
