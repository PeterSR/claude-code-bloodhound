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
	FilesParsed       int
	TurnsAdded        int
	CompactionsAdded  int
	Errors            []string
	ElapsedS          float64
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

	for _, p := range files {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		st.FilesScanned++

		info, err := os.Stat(p)
		if err != nil {
			st.Errors = append(st.Errors, fmt.Sprintf("stat %s: %v", p, err))
			continue
		}
		if info.Size() < opts.MinFileSize {
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

		fr, err := parseFile(p)
		if err != nil {
			st.Errors = append(st.Errors, fmt.Sprintf("parse %s: %v", p, err))
			continue
		}
		if len(fr.Turns) == 0 && len(fr.Compactions) == 0 {
			// Nothing useful; record mtime so we skip next time.
			_ = s.RecordIngestedFile(ctx, store.IngestedFileRecord{
				PathHash:         hash,
				Path:             p,
				MTimeUnix:        info.ModTime().Unix(),
				LastIngestedTS:   time.Now().UTC().Format(time.RFC3339),
				TurnCount:        0,
				CompactionCount:  0,
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

func findSessionFiles(dir string) ([]string, error) {
	var out []string
	matches, err := filepath.Glob(filepath.Join(dir, "*", "*.jsonl"))
	if err != nil {
		return nil, err
	}
	out = append(out, matches...)
	return out, nil
}

func toStoreTurns(in []Turn) []store.TurnRow {
	out := make([]store.TurnRow, len(in))
	for i, t := range in {
		out[i] = store.TurnRow{
			SessionUUID:    t.SessionUUID,
			TurnIdx:        t.TurnIdx,
			TS:             t.TS,
			TSUnixMS:       t.TSUnixMS,
			Model:          t.Model,
			InputTokens:    t.InputTokens,
			OutputTokens:   t.OutputTokens,
			CacheRead:      t.CacheRead,
			CacheCreate5m:  t.CacheCreate5m,
			CacheCreate1h:  t.CacheCreate1h,
			GapS:           t.GapS,
			Classification: t.Classification,
			PostCompact:    t.PostCompact,
			Project:        t.Project,
			SourcePathHash: t.SourcePathHash,
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
