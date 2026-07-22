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
		// human message (see the field doc). A subagent transcript has no
		// such correlation between file size and whether real spend
		// happened: a one-exchange subagent invocation can be small and
		// still carry a real API charge, so the guard only applies to
		// top-level files.
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
			sa = &subagentContext{parentUUID: sf.parentUUID}
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
	// isSubagent marks a file found one level deeper, at
	// <project>/<parent-uuid>/subagents/<file>.jsonl, rather than
	// <project>/<file>.jsonl.
	isSubagent bool
	// parentUUID is the directory two levels up from a subagent file (the
	// session that dispatched it). Unset for a top-level file.
	parentUUID string
}

// uuidRE matches a canonical 8-4-4-4-12 hex UUID. Used to validate a
// subagent's parent-directory name before trusting it as a link.
var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func looksLikeUUID(s string) bool {
	return uuidRE.MatchString(s)
}

// findSessionFiles walks both the top-level session layout Claude Code has
// always used (<project>/<session>.jsonl) and subagent transcripts, written
// one level deeper under a "subagents" directory named for the session that
// dispatched them (<project>/<parent-uuid>/subagents/agent-*.jsonl). The two
// patterns are independent globs rather than one recursive walk, so adding
// the second can't change what the first matches.
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
		// p = <project>/<parent-uuid>/subagents/<file>.jsonl, so the parent
		// UUID is the directory name one level above "subagents".
		parentUUID := filepath.Base(filepath.Dir(filepath.Dir(p)))
		out = append(out, sessionFile{path: p, isSubagent: true, parentUUID: parentUUID})
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
