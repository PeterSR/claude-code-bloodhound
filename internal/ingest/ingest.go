// Package ingest walks the on-disk JSONL session files Claude Code maintains,
// parses them, and writes turns + compaction events into the local store.
//
// Ingestion is mtime-gated: a file whose mtime hasn't changed since the last
// successful ingest is skipped. Pass Force to re-process everything.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/config"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

// Options for Run.
type Options struct {
	ProjectsDir string // empty => default from config.ClaudeProjectsDir
	MinFileSize int64  // skip files smaller than this (default 5_000)
	Force       bool   // re-ingest even if mtime is unchanged
}

// Stats summarises what one Run did.
type Stats struct {
	FilesScanned      int
	FilesSkippedMtime int
	FilesSkippedSmall int
	FilesSkippedTrail int
	// FilesSkippedBadParent counts subagent files whose containing directory
	// name didn't look like a session UUID: rather than trust a malformed
	// directory as a parent link, we skip the file and record why in Errors.
	FilesSkippedBadParent int
	FilesParsed           int
	TurnsAdded            int
	CompactionsAdded      int
	Errors                []string
	ElapsedS              float64
}

// Run is the one-shot ingester. Idempotent; safe to call concurrently with
// the rest of the system because it uses single-file transactions.
func Run(ctx context.Context, s *store.Store, opts Options) (Stats, error) {
	t0 := time.Now()
	st := Stats{}

	if opts.ProjectsDir == "" {
		dir, err := config.ClaudeProjectsDir()
		if err != nil {
			return st, err
		}
		opts.ProjectsDir = dir
	}
	if opts.MinFileSize == 0 {
		opts.MinFileSize = 5_000
	}

	files, err := findSessionFiles(opts.ProjectsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			st.ElapsedS = time.Since(t0).Seconds()
			return st, nil
		}
		return st, err
	}

	// Trail's own analyzer runs as `claude` with a known session-id and
	// writes JSONL like any other session. Skip those files entirely so
	// the analyzer's turns never enter the turns table — Trail cost is
	// attributed separately via trail_runs. Loaded once per ingest run.
	trailSkip, err := s.TrailSessionUUIDs(ctx)
	if err != nil {
		// Non-fatal: worst case a few analyzer turns leak into stats.
		trailSkip = nil
	}

	for _, sf := range files {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		st.FilesScanned++
		p := sf.path

		// A subagent transcript's parent link comes entirely from its
		// directory name, so a garbled one (a Claude Code version change,
		// a hand-edited directory, anything) has no safe fallback: writing
		// a turn with a made-up parent would misattribute real spend rather
		// than just miss it. Skip and say why, the same way a stat or parse
		// error does below.
		if sf.isSubagent && !looksLikeUUID(sf.parentUUID) {
			st.FilesSkippedBadParent++
			st.Errors = append(st.Errors, fmt.Sprintf(
				"subagent %s: parent dir %q doesn't look like a session UUID, skipping", p, sf.parentUUID))
			continue
		}

		// Trail's analyzer session is excluded by session UUID (see below);
		// a subagent it dispatched has to be excluded by its PARENT's UUID
		// instead, since the subagent's own id (the filename stem) never
		// appears in trail_runs.
		trailKey := sessionUUIDFromPath(p)
		if sf.isSubagent {
			trailKey = sf.parentUUID
		}
		if trailSkip[trailKey] {
			st.FilesSkippedTrail++
			continue
		}

		info, err := os.Stat(p)
		if err != nil {
			st.Errors = append(st.Errors, fmt.Sprintf("stat %s: %v", p, err))
			continue
		}
		// MinFileSize guards against wasting a parse + transaction on an
		// abandoned top-level session that's little more than the initial
		// human message (see the field doc). A subagent transcript (Task-
		// or Workflow-tool, same exemption either way) has no such
		// correlation between file size and whether real spend happened:
		// a one-exchange subagent invocation can be small and still carry
		// a real API charge, so the guard only applies to top-level files.
		if !sf.isSubagent && info.Size() < opts.MinFileSize {
			st.FilesSkippedSmall++
			continue
		}

		hash := hashPath(p)
		if !opts.Force {
			prev, err := s.GetIngestedMTime(ctx, hash)
			if err == nil && prev == info.ModTime().Unix() {
				st.FilesSkippedMtime++
				continue
			}
		}

		var sa *subagentContext
		if sf.isSubagent {
			sa = &subagentContext{parentUUID: sf.parentUUID, project: sf.project}
		}
		fr, err := parseFile(p, sa)
		if err != nil {
			st.Errors = append(st.Errors, fmt.Sprintf("parse %s: %v", p, err))
			continue
		}
		if len(fr.Turns) == 0 && len(fr.Compactions) == 0 {
			// Nothing useful; record mtime so we skip next time.
			_ = s.RecordIngestedFile(ctx, store.IngestedFileRecord{
				PathHash:        hash,
				Path:            p,
				MTimeUnix:       info.ModTime().Unix(),
				LastIngestedTS:  time.Now().UTC().Format(time.RFC3339),
				TurnCount:       0,
				CompactionCount: 0,
			})
			st.FilesParsed++
			continue
		}

		if err := s.ReplaceSessionData(ctx, store.SessionPersist{
			SessionUUID: fr.SessionUUID,
			Project:     fr.Project,
			Turns:       toStoreTurns(fr.Turns),
			Compactions: toStoreCompactions(fr.Compactions),
			UserPrompts: toStoreUserPrompts(fr.UserPrompts),
		}); err != nil {
			st.Errors = append(st.Errors, fmt.Sprintf("persist %s: %v", p, err))
			continue
		}

		if err := s.RecordIngestedFile(ctx, store.IngestedFileRecord{
			PathHash:        hash,
			Path:            p,
			MTimeUnix:       info.ModTime().Unix(),
			LastIngestedTS:  time.Now().UTC().Format(time.RFC3339),
			TurnCount:       len(fr.Turns),
			CompactionCount: len(fr.Compactions),
		}); err != nil {
			st.Errors = append(st.Errors, fmt.Sprintf("ingested-file %s: %v", p, err))
		}

		st.FilesParsed++
		st.TurnsAdded += len(fr.Turns)
		st.CompactionsAdded += len(fr.Compactions)
	}

	st.ElapsedS = time.Since(t0).Seconds()
	return st, nil
}

// sessionUUIDFromPath derives the session UUID from a JSONL path the
// same way parseFile does (basename minus ".jsonl"), for the trail-skip
// check without opening the file.
func sessionUUIDFromPath(p string) string {
	return strings.TrimSuffix(filepath.Base(p), ".jsonl")
}

// sessionFile is one discovered JSONL transcript.
type sessionFile struct {
	path string
	// isSubagent marks a file found one or more levels deeper than a
	// top-level session file: either the Task-tool shape
	// (<project>/<parent-uuid>/subagents/<file>.jsonl) or the Workflow-tool
	// shape one level deeper still
	// (<project>/<parent-uuid>/subagents/workflows/<wf-id>/agent-*.jsonl).
	// The two share one flag rather than a second isWorkflowAgent bool
	// because everything downstream (the MinFileSize exemption, the
	// trail-skip lookup, the parent-UUID validation, the rollup) treats
	// them identically; only findSessionFiles needs to know which shape it
	// found, to compute parentUUID/project at the right depth.
	isSubagent bool
	// parentUUID is the session that dispatched this file: the directory
	// named for a session UUID, however many levels up the specific shape
	// puts it. Unset for a top-level file.
	parentUUID string
	// project is the containing project directory. Always set; computed
	// here (where the matched shape, and so the exact depth, is already
	// known) rather than re-derived from path depth in parseFile, which
	// can't tell a Task-tool subagent from a Workflow-tool one just by
	// looking at the path.
	project string
}

// upN walks p up n directories via repeated filepath.Dir. Used to reach an
// ancestor directory a known number of levels above a discovered file,
// where "known" comes from which glob in findSessionFiles matched.
func upN(p string, n int) string {
	for i := 0; i < n; i++ {
		p = filepath.Dir(p)
	}
	return p
}

// uuidRE matches a canonical 8-4-4-4-12 hex UUID. Used to validate a
// subagent's parent-directory name before trusting it as a link.
var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func looksLikeUUID(s string) bool {
	return uuidRE.MatchString(s)
}

// findSessionFiles walks three transcript layouts Claude Code writes:
//
//   - the top-level session layout it has always used,
//     <project>/<session>.jsonl;
//   - Task-tool subagent transcripts, one level deeper under a "subagents"
//     directory named for the session that dispatched them,
//     <project>/<parent-uuid>/subagents/agent-*.jsonl;
//   - Workflow-tool agent transcripts, the same idea one level deeper
//     still, grouped under a directory per workflow run,
//     <project>/<parent-uuid>/subagents/workflows/<wf-id>/agent-*.jsonl.
//
// The three patterns are independent globs rather than one recursive walk,
// so adding the second and third could never change what the first matches.
func findSessionFiles(dir string) ([]sessionFile, error) {
	var out []sessionFile

	top, err := filepath.Glob(filepath.Join(dir, "*", "*.jsonl"))
	if err != nil {
		return nil, err
	}
	for _, p := range top {
		out = append(out, sessionFile{path: p})
	}

	sub, err := filepath.Glob(filepath.Join(dir, "*", "*", "subagents", "*.jsonl"))
	if err != nil {
		return nil, err
	}
	for _, p := range sub {
		// p = <project>/<parent-uuid>/subagents/<file>.jsonl
		parentDir := upN(p, 2) // <project>/<parent-uuid>
		out = append(out, sessionFile{
			path:       p,
			isSubagent: true,
			parentUUID: filepath.Base(parentDir),
			project:    filepath.Base(filepath.Dir(parentDir)),
		})
	}

	// Workflow agents: one per agent the Workflow tool dispatched, grouped
	// under a directory named for the workflow run (<wf-id>). The same
	// directory also holds journal.jsonl, the workflow's own run journal
	// (start/result events keyed by agentId, written by the workflow
	// runner itself, not a transcript of anything an assistant said).
	// Verified live against every journal.jsonl on disk: zero "assistant"
	// records, no token usage object anywhere in the file. The glob is
	// "agent-*.jsonl" rather than "*.jsonl" specifically to exclude it, a
	// deliberate skip rather than relying on it happening to parse to
	// zero turns if it were ever fed through parseFile.
	//
	// Reuses isSubagent rather than adding an isWorkflowAgent flag: a
	// workflow agent attributes to its parentUUID exactly like a Task-tool
	// subagent does (same rollup, same trail-skip rule, same MinFileSize
	// exemption, see their uses in Run), so there is nothing for a
	// separate flag to distinguish downstream of here.
	wf, err := filepath.Glob(filepath.Join(dir, "*", "*", "subagents", "workflows", "*", "agent-*.jsonl"))
	if err != nil {
		return nil, err
	}
	for _, p := range wf {
		// p = <project>/<parent-uuid>/subagents/workflows/<wf-id>/agent-x.jsonl
		parentDir := upN(p, 4) // <project>/<parent-uuid>
		out = append(out, sessionFile{
			path:       p,
			isSubagent: true,
			parentUUID: filepath.Base(parentDir),
			project:    filepath.Base(filepath.Dir(parentDir)),
		})
	}

	return out, nil
}

func toStoreTurns(in []Turn) []store.TurnRow {
	out := make([]store.TurnRow, len(in))
	for i, t := range in {
		out[i] = store.TurnRow{
			SessionUUID:       t.SessionUUID,
			TurnIdx:           t.TurnIdx,
			TS:                t.TS,
			TSUnixMS:          t.TSUnixMS,
			Model:             t.Model,
			InputTokens:       t.InputTokens,
			OutputTokens:      t.OutputTokens,
			CacheRead:         t.CacheRead,
			CacheCreate5m:     t.CacheCreate5m,
			CacheCreate1h:     t.CacheCreate1h,
			GapS:              t.GapS,
			Classification:    t.Classification,
			PostCompact:       t.PostCompact,
			Project:           t.Project,
			SourcePathHash:    t.SourcePathHash,
			ParentSessionUUID: t.ParentSessionUUID,
			Cwd:               t.Cwd,
		}
	}
	return out
}

func toStoreUserPrompts(in []UserPrompt) []store.UserPromptRow {
	out := make([]store.UserPromptRow, len(in))
	for i, p := range in {
		out[i] = store.UserPromptRow{
			SessionUUID: p.SessionUUID,
			TSUnixMS:    p.TSUnixMS,
			TextPreview: p.TextPreview,
		}
	}
	return out
}

func toStoreCompactions(in []Compaction) []store.CompactionRow {
	out := make([]store.CompactionRow, len(in))
	for i, c := range in {
		out[i] = store.CompactionRow{
			SessionUUID:      c.SessionUUID,
			TS:               c.TS,
			TSUnixMS:         c.TSUnixMS,
			PrefixTokensEst:  c.PrefixTokensEst,
			SummaryTokensEst: c.SummaryTokensEst,
			GapToPrevS:       c.GapToPrevS,
			CacheState:       c.CacheState,
			Confirmed:        c.Confirmed,
			ConfirmReason:    c.ConfirmReason,
			Project:          c.Project,
		}
	}
	return out
}
