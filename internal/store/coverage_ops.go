package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/PeterSR/claude-code-bloodhound/internal/config"
)

// CoverageShape identifies one on-disk transcript layout, recognised or
// not, that ingest.Run's discovery walk could encounter.
type CoverageShape string

const (
	// ShapeTopLevel is <project>/<session>.jsonl.
	ShapeTopLevel CoverageShape = "top_level"
	// ShapeSubagent is <project>/<parent-uuid>/subagents/<file>.jsonl, a
	// Task-tool subagent transcript.
	ShapeSubagent CoverageShape = "subagent"
	// ShapeWorkflowAgent is
	// <project>/<parent-uuid>/subagents/workflows/<wf-id>/agent-*.jsonl, a
	// Workflow-tool agent transcript.
	ShapeWorkflowAgent CoverageShape = "workflow_agent"
	// ShapeUnrecognized is any .jsonl file under the projects dir that
	// doesn't match one of the three shapes above. This is the shape that
	// makes the check worth having: if Claude Code introduces a fourth
	// nesting layout, ingest won't glob for it and files land here instead
	// of vanishing from the count the way they'd vanish from ingest's own
	// walk. An unrecognised file is never treated as a legitimate skip (see
	// computeCoverage), so it always counts as Missing.
	ShapeUnrecognized CoverageShape = "unrecognized"
)

// coverageShapes is the fixed row order CheckCoverage reports in, so
// callers (doctor's human and --json output alike) always see all four
// shapes, including ones with zero files on disk.
var coverageShapes = []CoverageShape{ShapeTopLevel, ShapeSubagent, ShapeWorkflowAgent, ShapeUnrecognized}

// CoverageRow is the on-disk-vs-ingested tally for one shape.
type CoverageRow struct {
	Shape CoverageShape `json:"shape"`
	// OnDisk is every transcript file of this shape found under the
	// projects dir, including ones legitimately absent from the database
	// (see Skipped).
	OnDisk int `json:"on_disk"`
	// Ingested is how many of those OnDisk files have a row in
	// ingested_files: ingest has actually looked at the file, whether or
	// not that look produced any turns. A file that legitimately parses to
	// zero billable turns still gets a row there (see ingest.Run's
	// "nothing useful" branch), so it counts as Ingested here too, not
	// Missing.
	Ingested int `json:"ingested"`
	// Skipped is OnDisk files absent from ingested_files for a reason
	// ingest.Run treats as deliberate rather than a gap: the file belongs
	// to a Trail analyzer session, or (top-level shape only) it sits below
	// MinFileSize. These must not inflate Missing, or the check becomes
	// noise people learn to ignore.
	Skipped int `json:"skipped"`
	// Missing is OnDisk minus Ingested minus Skipped: files this check has
	// no explanation for. Nonzero here is the loud signal: either ingest
	// simply hasn't run since the file appeared, or (ShapeUnrecognized)
	// ingest doesn't know this layout exists yet.
	Missing int `json:"missing"`
}

// CoverageReport is every shape's tally, computed against one projects
// dir.
type CoverageReport struct {
	ProjectsDir string        `json:"projects_dir"`
	Rows        []CoverageRow `json:"rows"`
}

// AnyMissing reports whether any shape has a nonzero Missing count. This
// is the signal doctor uses to decide whether the coverage section needs
// to shout.
func (r CoverageReport) AnyMissing() bool {
	for _, row := range r.Rows {
		if row.Missing > 0 {
			return true
		}
	}
	return false
}

// TotalMissing sums Missing across every shape.
func (r CoverageReport) TotalMissing() int {
	total := 0
	for _, row := range r.Rows {
		total += row.Missing
	}
	return total
}

// CoverageOptions configures CheckCoverage.
type CoverageOptions struct {
	// ProjectsDir overrides the Claude Code projects dir; empty uses
	// config.ClaudeProjectsDir(), same fallback ingest.Options uses.
	ProjectsDir string
	// MinFileSize mirrors ingest.Options.MinFileSize: a top-level file
	// below this is a legitimate skip, not a gap. Should be passed the
	// same value the caller actually runs ingest with, or left 0 to use
	// ingest's own default (5,000 bytes).
	MinFileSize int64
}

// classifiedPath is the result of classifying one path relative to the
// projects dir.
type classifiedPath struct {
	shape CoverageShape
	// trailKey is the UUID to check against trail_runs when deciding
	// whether an absent ingested_files row is a legitimate Trail skip: the
	// file's own session UUID for a top-level file (mirrors ingest.Run's
	// sessionUUIDFromPath), or its parent's UUID for a subagent /
	// workflow-agent file (mirrors ingest.Run's trailKey override for
	// sf.isSubagent). Empty for ShapeUnrecognized, which has no such link
	// to check.
	trailKey string
	// isJournal marks the workflow's own run journal (journal.jsonl,
	// sitting alongside workflow-agent files) as excluded outright: not a
	// transcript, so it must not be counted under any shape, including
	// ShapeUnrecognized. See its use in the caller.
	isJournal bool
}

// classifyTranscriptPath maps rel, a '/'-separated path relative to the
// Claude Code projects dir, to the shape ingest.Run's discovery walk
// would (or wouldn't) find it under. Pure and independent of any actual
// disk walk or database, so the three known shapes, the journal.jsonl
// exclusion, and the unrecognised catch-all can all be tested directly
// against literal path strings, including hypothetical future layouts.
//
// Deliberately does NOT reuse internal/ingest's own path matching: ingest
// already imports store, so the reverse import would cycle. The
// classification rules are simple enough (three fixed depths plus a
// filename check) that duplicating them here, in the one place a genuine
// divergence would actually matter, is cheaper than restructuring package
// boundaries to share them.
func classifyTranscriptPath(rel string) classifiedPath {
	parts := strings.Split(rel, "/")

	switch {
	case len(parts) == 2 && strings.HasSuffix(parts[1], ".jsonl"):
		// <project>/<session>.jsonl
		return classifiedPath{shape: ShapeTopLevel, trailKey: strings.TrimSuffix(parts[1], ".jsonl")}

	case len(parts) == 4 && parts[2] == "subagents" && strings.HasSuffix(parts[3], ".jsonl"):
		// <project>/<parent-uuid>/subagents/<file>.jsonl
		return classifiedPath{shape: ShapeSubagent, trailKey: parts[1]}

	case len(parts) == 6 && parts[2] == "subagents" && parts[3] == "workflows":
		// <project>/<parent-uuid>/subagents/workflows/<wf-id>/<file>.jsonl
		if parts[5] == "journal.jsonl" {
			// The workflow's own run journal (start/result events keyed by
			// agentId), not an agent transcript. ingest.Run's glob for
			// this shape is "agent-*.jsonl" specifically so it never sees
			// this file; excluded here the same explicit way, rather than
			// falling through to ShapeUnrecognized, which would make every
			// workflow run report a permanent phantom gap.
			return classifiedPath{isJournal: true}
		}
		if strings.HasPrefix(parts[5], "agent-") && strings.HasSuffix(parts[5], ".jsonl") {
			return classifiedPath{shape: ShapeWorkflowAgent, trailKey: parts[1]}
		}
		return classifiedPath{shape: ShapeUnrecognized}

	default:
		// Anything else: wrong depth, wrong directory names, a shape
		// ingest doesn't know about yet. Counts as uncovered rather than
		// invisible; see ShapeUnrecognized's doc.
		return classifiedPath{shape: ShapeUnrecognized}
	}
}

// classifiedFile is one on-disk transcript file, already classified and
// stat'd, ready for computeCoverage to reconcile against the database.
type classifiedFile struct {
	pathHash string
	shape    CoverageShape
	trailKey string
	size     int64
}

// hashTranscriptPath mirrors internal/ingest's own hashPath: sha256 hex of
// the absolute path, the same key ingested_files.path_hash is written
// under. Duplicated rather than exported from internal/ingest to avoid a
// store -> ingest import (ingest already imports store; the reverse would
// cycle) for three lines neither side has reason to change independently
// of the other. If they ever did need to diverge, they would no longer be
// computing the same key anyway, so sharing wouldn't help.
func hashTranscriptPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	sum := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(sum[:])
}

// computeCoverage reconciles a set of on-disk transcript files against
// what ingest has recorded, shape by shape. Pure and DB-free: the shape
// classification and the legitimate-skip carve-outs (Trail, MinFileSize)
// are exercised directly here, without a real database or real files on
// disk. CheckCoverage is the thin, untested-by-choice wrapper that
// actually walks the disk and reads the tables this needs.
func computeCoverage(files []classifiedFile, ingestedHashes, trailUUIDs map[string]bool, minFileSize int64) CoverageReport {
	tallies := make(map[CoverageShape]*CoverageRow, len(coverageShapes))
	for _, shape := range coverageShapes {
		tallies[shape] = &CoverageRow{Shape: shape}
	}

	for _, f := range files {
		row, ok := tallies[f.shape]
		if !ok {
			// Defensive: classifyTranscriptPath only ever returns shapes
			// in coverageShapes. Fold anything else into Unrecognized
			// rather than dropping it.
			row = tallies[ShapeUnrecognized]
		}
		row.OnDisk++

		if ingestedHashes[f.pathHash] {
			row.Ingested++
			continue
		}
		if f.trailKey != "" && trailUUIDs[f.trailKey] {
			row.Skipped++
			continue
		}
		if f.shape == ShapeTopLevel && f.size < minFileSize {
			row.Skipped++
			continue
		}
		row.Missing++
	}

	report := CoverageReport{}
	for _, shape := range coverageShapes {
		report.Rows = append(report.Rows, *tallies[shape])
	}
	return report
}

// CheckCoverage walks ProjectsDir, classifies every transcript file found
// there into a shape, and cross-references each one against
// ingested_files (and, where the shape calls for it, trail_runs and
// MinFileSize) to report how many are actually accounted for.
//
// This exists because `unattributed` cannot serve as a coverage canary:
// attribution divides *measured meter movement* pro-rata among whatever
// turns ingest can see, so missing spend doesn't show up as a gap, it
// just inflates the visible rows' share of the same movement. A whole
// nesting level can go uningested, as the Workflow-tool shape did until
// this check, while unattributed stays near zero throughout. This check
// instead counts files, independent of anything attribution computes.
func (s *Store) CheckCoverage(ctx context.Context, opts CoverageOptions) (CoverageReport, error) {
	projectsDir := opts.ProjectsDir
	if projectsDir == "" {
		dir, err := config.ClaudeProjectsDir()
		if err != nil {
			return CoverageReport{}, err
		}
		projectsDir = dir
	}
	minFileSize := opts.MinFileSize
	if minFileSize == 0 {
		minFileSize = 5_000 // mirrors ingest.Options' own default
	}

	var files []classifiedFile
	if _, statErr := os.Stat(projectsDir); statErr == nil {
		walkErr := filepath.WalkDir(projectsDir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
				return nil
			}
			rel, relErr := filepath.Rel(projectsDir, p)
			if relErr != nil {
				return nil
			}
			cp := classifyTranscriptPath(filepath.ToSlash(rel))
			if cp.isJournal {
				return nil
			}
			var size int64
			if info, infoErr := d.Info(); infoErr == nil {
				size = info.Size()
			}
			files = append(files, classifiedFile{
				pathHash: hashTranscriptPath(p),
				shape:    cp.shape,
				trailKey: cp.trailKey,
				size:     size,
			})
			return nil
		})
		if walkErr != nil {
			return CoverageReport{}, walkErr
		}
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return CoverageReport{}, statErr
	}
	// A missing projects dir (fresh machine, wrong override) reports zero
	// files of every shape rather than erroring, the same tolerance
	// ingest.Run applies to the same condition.

	ingestedHashes, err := s.allIngestedPathHashes(ctx)
	if err != nil {
		return CoverageReport{}, err
	}
	trailUUIDs, err := s.TrailSessionUUIDs(ctx)
	if err != nil {
		// Non-fatal, the same tolerance ingest.Run applies: worst case a
		// handful of Trail-owned files report Missing instead of Skipped,
		// which is loud but not wrong (nothing claimed they weren't
		// found).
		trailUUIDs = nil
	}

	report := computeCoverage(files, ingestedHashes, trailUUIDs, minFileSize)
	report.ProjectsDir = projectsDir
	return report, nil
}

// allIngestedPathHashes loads the full path_hash set from ingested_files
// once, so CheckCoverage can look up membership in memory per file rather
// than issuing one query per file on disk.
func (s *Store) allIngestedPathHashes(ctx context.Context) (map[string]bool, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT path_hash FROM ingested_files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return out, err
		}
		out[h] = true
	}
	return out, rows.Err()
}
