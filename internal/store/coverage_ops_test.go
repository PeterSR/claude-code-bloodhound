package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestClassifyTranscriptPath covers every shape ingest.Run's discovery walk
// recognises, the journal.jsonl exclusion, and the unrecognised catch-all:
// the case that makes this check worth having (see ShapeUnrecognized's
// doc). A path shape nothing above has ever heard of must still come back
// classified, not silently dropped.
func TestClassifyTranscriptPath(t *testing.T) {
	const parentUUID = "a850d051-2b0d-455b-991c-a0a434be269f"

	cases := []struct {
		name          string
		rel           string
		wantShape     CoverageShape
		wantTrailKey  string
		wantIsJournal bool
	}{
		{
			name:         "top level",
			rel:          "proj1/session-abc12345.jsonl",
			wantShape:    ShapeTopLevel,
			wantTrailKey: "session-abc12345",
		},
		{
			name:         "subagent",
			rel:          "proj1/" + parentUUID + "/subagents/agent-x.jsonl",
			wantShape:    ShapeSubagent,
			wantTrailKey: parentUUID,
		},
		{
			name:         "workflow agent",
			rel:          "proj1/" + parentUUID + "/subagents/workflows/wf_abc123/agent-x.jsonl",
			wantShape:    ShapeWorkflowAgent,
			wantTrailKey: parentUUID,
		},
		{
			name:          "workflow journal excluded, not a transcript",
			rel:           "proj1/" + parentUUID + "/subagents/workflows/wf_abc123/journal.jsonl",
			wantIsJournal: true,
		},
		{
			name:      "workflow dir, unrecognised filename",
			rel:       "proj1/" + parentUUID + "/subagents/workflows/wf_abc123/summary.jsonl",
			wantShape: ShapeUnrecognized,
		},
		{
			name:      "four deep but not under subagents",
			rel:       "proj1/" + parentUUID + "/notes/file.jsonl",
			wantShape: ShapeUnrecognized,
		},
		{
			name:      "orphan file directly under projects dir",
			rel:       "orphan.jsonl",
			wantShape: ShapeUnrecognized,
		},
		{
			name:      "hypothetical fifth-level shape",
			rel:       "proj1/" + parentUUID + "/subagents/deeper/still/more/agent-x.jsonl",
			wantShape: ShapeUnrecognized,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyTranscriptPath(c.rel)
			if got.isJournal != c.wantIsJournal {
				t.Errorf("isJournal = %v, want %v", got.isJournal, c.wantIsJournal)
			}
			if c.wantIsJournal {
				return // shape/trailKey are irrelevant once excluded
			}
			if got.shape != c.wantShape {
				t.Errorf("shape = %q, want %q", got.shape, c.wantShape)
			}
			if got.trailKey != c.wantTrailKey {
				t.Errorf("trailKey = %q, want %q", got.trailKey, c.wantTrailKey)
			}
		})
	}
}

// TestComputeCoverage_IngestedCountsAsCovered covers the base case: a file
// present in ingested_files (regardless of shape) is Ingested, not Missing.
func TestComputeCoverage_IngestedCountsAsCovered(t *testing.T) {
	files := []classifiedFile{
		{pathHash: "h1", shape: ShapeTopLevel},
		{pathHash: "h2", shape: ShapeSubagent},
		{pathHash: "h3", shape: ShapeWorkflowAgent},
	}
	ingested := map[string]bool{"h1": true, "h2": true, "h3": true}

	report := computeCoverage(files, ingested, nil, 5_000)

	for _, row := range report.Rows {
		if row.Shape == ShapeUnrecognized {
			continue
		}
		if row.OnDisk != 1 || row.Ingested != 1 || row.Missing != 0 || row.Skipped != 0 {
			t.Errorf("shape %s: got %+v, want on_disk=1 ingested=1 missing=0 skipped=0", row.Shape, row)
		}
	}
}

// TestComputeCoverage_TrailSkipDoesNotCountAsMissing covers the Trail
// carve-out for all three named shapes: a top-level file is looked up by
// its own UUID, a subagent/workflow-agent file by its PARENT's UUID (it has
// no session identity of its own that would appear in trail_runs).
func TestComputeCoverage_TrailSkipDoesNotCountAsMissing(t *testing.T) {
	const trailUUID = "trail-session-uuid"
	files := []classifiedFile{
		{pathHash: "h1", shape: ShapeTopLevel, trailKey: trailUUID},
		{pathHash: "h2", shape: ShapeSubagent, trailKey: trailUUID},
		{pathHash: "h3", shape: ShapeWorkflowAgent, trailKey: trailUUID},
	}
	trailUUIDs := map[string]bool{trailUUID: true}

	report := computeCoverage(files, nil, trailUUIDs, 5_000)

	for _, row := range report.Rows {
		if row.Shape == ShapeUnrecognized {
			continue
		}
		if row.Skipped != 1 || row.Missing != 0 {
			t.Errorf("shape %s: got %+v, want skipped=1 missing=0", row.Shape, row)
		}
	}
}

// TestComputeCoverage_MinFileSizeOnlyExemptsTopLevel covers the asymmetry
// ingest.Run itself applies: a small top-level file is a legitimate skip,
// but a small subagent or workflow-agent file is not (a one-exchange
// subagent invocation can legitimately be tiny and still carry real
// spend; see the comment on ingest.Run's MinFileSize check). If this ever
// regresses to exempting every shape, a genuinely missing small subagent
// file would silently stop being reported.
func TestComputeCoverage_MinFileSizeOnlyExemptsTopLevel(t *testing.T) {
	files := []classifiedFile{
		{pathHash: "h1", shape: ShapeTopLevel, size: 10},
		{pathHash: "h2", shape: ShapeSubagent, size: 10},
		{pathHash: "h3", shape: ShapeWorkflowAgent, size: 10},
	}

	report := computeCoverage(files, nil, nil, 5_000)

	byShape := map[CoverageShape]CoverageRow{}
	for _, row := range report.Rows {
		byShape[row.Shape] = row
	}

	if got := byShape[ShapeTopLevel]; got.Skipped != 1 || got.Missing != 0 {
		t.Errorf("top-level small file: got %+v, want skipped=1 missing=0", got)
	}
	if got := byShape[ShapeSubagent]; got.Skipped != 0 || got.Missing != 1 {
		t.Errorf("subagent small file: got %+v, want skipped=0 missing=1 (no size exemption)", got)
	}
	if got := byShape[ShapeWorkflowAgent]; got.Skipped != 0 || got.Missing != 1 {
		t.Errorf("workflow-agent small file: got %+v, want skipped=0 missing=1 (no size exemption)", got)
	}
}

// TestComputeCoverage_UnrecognizedIsAlwaysMissingUnlessIngested is the
// point of the whole check: an unrecognised shape gets no Trail or
// MinFileSize carve-out at all, so the only way it avoids Missing is an
// actual ingested_files row.
func TestComputeCoverage_UnrecognizedIsAlwaysMissingUnlessIngested(t *testing.T) {
	files := []classifiedFile{
		{pathHash: "h1", shape: ShapeUnrecognized, size: 1}, // tiny, but no size exemption applies
		{pathHash: "h2", shape: ShapeUnrecognized},
	}
	ingested := map[string]bool{"h2": true}

	report := computeCoverage(files, ingested, nil, 5_000)

	for _, row := range report.Rows {
		if row.Shape != ShapeUnrecognized {
			continue
		}
		if row.OnDisk != 2 || row.Ingested != 1 || row.Missing != 1 || row.Skipped != 0 {
			t.Errorf("unrecognized: got %+v, want on_disk=2 ingested=1 missing=1 skipped=0", row)
		}
	}
}

// TestComputeCoverage_AlwaysReportsAllFourShapes covers the doctor
// requirement that the coverage table always shows all four rows, even
// when nothing of a given shape is on disk (a fresh install, or a shape
// that simply hasn't occurred yet): a shape absent from the report would
// be indistinguishable from "not checked", which is exactly the blind spot
// this feature exists to close.
func TestComputeCoverage_AlwaysReportsAllFourShapes(t *testing.T) {
	report := computeCoverage(nil, nil, nil, 5_000)
	if len(report.Rows) != 4 {
		t.Fatalf("got %d rows, want 4 (one per shape, even with nothing on disk): %+v", len(report.Rows), report.Rows)
	}
	if report.AnyMissing() {
		t.Errorf("AnyMissing() = true for an empty report, want false")
	}
	if report.TotalMissing() != 0 {
		t.Errorf("TotalMissing() = %d, want 0", report.TotalMissing())
	}
}

// TestCheckCoverage_EndToEnd drives the real walk-plus-database path
// against a fabricated projects dir and a real (temp) store, covering one
// file of each disposition: ingested, Trail-skipped, small-and-skipped
// (top-level only), genuinely missing, and unrecognised. This is the test
// that would have caught the original bug: before the Workflow-tool glob
// existed, a workflow-agent file here would have landed in Missing instead
// of being discovered at all. CheckCoverage doesn't depend on ingest's own
// glob, so it catches that independently.
func TestCheckCoverage_EndToEnd(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const (
		trailUUID  = "22222222-2222-2222-2222-222222222222"
		parentUUID = "33333333-3333-3333-3333-333333333333"
	)

	projectsDir := t.TempDir()
	longEnough := `{"padding":"` + strings.Repeat("x", 40) + `"}`

	topIngested := filepath.Join(projectsDir, "proj1", "session-ingested.jsonl")
	topTrail := filepath.Join(projectsDir, "proj1", trailUUID+".jsonl")
	topSmall := filepath.Join(projectsDir, "proj1", "session-tiny.jsonl")
	topMissing := filepath.Join(projectsDir, "proj1", "session-missing.jsonl")
	subMissing := filepath.Join(projectsDir, "proj1", parentUUID, "subagents", "agent-missing.jsonl")
	wfIngested := filepath.Join(projectsDir, "proj1", parentUUID, "subagents", "workflows", "wf_1", "agent-ingested.jsonl")
	wfJournal := filepath.Join(projectsDir, "proj1", parentUUID, "subagents", "workflows", "wf_1", "journal.jsonl")
	unrecognized := filepath.Join(projectsDir, "proj1", parentUUID, "notes", "file.jsonl")

	for _, p := range []string{topIngested, topTrail, topMissing, subMissing, wfIngested, unrecognized} {
		writeTestFile(t, p, longEnough)
	}
	writeTestFile(t, topSmall, "{}")
	writeTestFile(t, wfJournal, `{"type":"started","agentId":"x"}`)

	for _, p := range []string{topIngested, wfIngested} {
		if err := s.RecordIngestedFile(ctx, IngestedFileRecord{
			PathHash:       hashTranscriptPath(p),
			Path:           p,
			MTimeUnix:      1,
			LastIngestedTS: "2026-01-01T00:00:00Z",
			TurnCount:      1,
		}); err != nil {
			t.Fatalf("RecordIngestedFile(%s): %v", p, err)
		}
	}

	if _, err := s.StartTrailRun(ctx, TrailRun{
		TrailSessionUUID:  trailUUID,
		TargetSessionUUID: "some-target",
		Mode:              "session",
		StartedUnixMS:     1,
	}); err != nil {
		t.Fatalf("StartTrailRun: %v", err)
	}

	report, err := s.CheckCoverage(ctx, CoverageOptions{ProjectsDir: projectsDir, MinFileSize: 20})
	if err != nil {
		t.Fatalf("CheckCoverage: %v", err)
	}

	byShape := map[CoverageShape]CoverageRow{}
	for _, row := range report.Rows {
		byShape[row.Shape] = row
	}

	top := byShape[ShapeTopLevel]
	if top.OnDisk != 4 {
		t.Errorf("top-level on_disk = %d, want 4 (ingested, trail, small, missing)", top.OnDisk)
	}
	if top.Ingested != 1 || top.Skipped != 2 || top.Missing != 1 {
		t.Errorf("top-level tally = %+v, want ingested=1 skipped=2 missing=1", top)
	}

	sub := byShape[ShapeSubagent]
	if sub.OnDisk != 1 || sub.Ingested != 0 || sub.Skipped != 0 || sub.Missing != 1 {
		t.Errorf("subagent tally = %+v, want on_disk=1 missing=1", sub)
	}

	wf := byShape[ShapeWorkflowAgent]
	if wf.OnDisk != 1 || wf.Ingested != 1 || wf.Missing != 0 {
		t.Errorf("workflow_agent tally = %+v, want on_disk=1 ingested=1 missing=0", wf)
	}

	unrec := byShape[ShapeUnrecognized]
	if unrec.OnDisk != 1 || unrec.Missing != 1 {
		t.Errorf("unrecognized tally = %+v, want on_disk=1 missing=1", unrec)
	}

	if report.ProjectsDir != projectsDir {
		t.Errorf("ProjectsDir = %q, want %q", report.ProjectsDir, projectsDir)
	}
	if !report.AnyMissing() {
		t.Errorf("AnyMissing() = false, want true (top-level + subagent + unrecognized each have a real gap)")
	}
	if got, want := report.TotalMissing(), 3; got != want {
		t.Errorf("TotalMissing() = %d, want %d", got, want)
	}
}

// TestCheckCoverage_NoProjectsDir covers the fresh-machine / wrong-override
// case: a nonexistent projects dir reports zero files everywhere rather
// than erroring, the same tolerance ingest.Run applies to the same
// condition.
func TestCheckCoverage_NoProjectsDir(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	report, err := s.CheckCoverage(ctx, CoverageOptions{
		ProjectsDir: filepath.Join(t.TempDir(), "does-not-exist"),
	})
	if err != nil {
		t.Fatalf("CheckCoverage: %v", err)
	}
	if report.AnyMissing() {
		t.Errorf("AnyMissing() = true for a nonexistent projects dir, want false")
	}
	for _, row := range report.Rows {
		if row.OnDisk != 0 {
			t.Errorf("shape %s: on_disk = %d, want 0", row.Shape, row.OnDisk)
		}
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
