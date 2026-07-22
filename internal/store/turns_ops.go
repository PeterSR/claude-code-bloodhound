package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// TurnRow mirrors the `turns` table.
type TurnRow struct {
	SessionUUID    string
	TurnIdx        int
	TS             string
	TSUnixMS       int64
	Model          string
	InputTokens    int
	OutputTokens   int
	CacheRead      int
	CacheCreate5m  int
	CacheCreate1h  int
	GapS           float64
	Classification string
	PostCompact    bool
	Project        string
	SourcePathHash string
	// ParentSessionUUID is "" for a normal top-level session, otherwise the
	// session UUID that dispatched this turn's subagent (see 0010_*.sql).
	ParentSessionUUID string
	// Cwd is the JSONL record's own working directory. Kept per-turn
	// (denormalized, like Project) because a subagent's cwd can legitimately
	// differ from its parent's project directory.
	Cwd string
}

// CompactionRow mirrors the `compactions` table.
type CompactionRow struct {
	SessionUUID      string
	TS               string
	TSUnixMS         int64
	PrefixTokensEst  int
	SummaryTokensEst int
	GapToPrevS       *float64
	CacheState       string
	Confirmed        bool
	ConfirmReason    string
	Project          string
}

// UserPromptRow mirrors the `user_prompts` table.
type UserPromptRow struct {
	SessionUUID string
	TSUnixMS    int64
	TextPreview string
}

// SessionPersist bundles the data ingested for a single session_uuid.
type SessionPersist struct {
	SessionUUID string
	Project     string
	Turns       []TurnRow
	Compactions []CompactionRow
	UserPrompts []UserPromptRow
}

// IngestedFileRecord tracks per-file mtime so we can skip unchanged files
// on subsequent ingest passes.
type IngestedFileRecord struct {
	PathHash        string
	Path            string
	MTimeUnix       int64
	LastIngestedTS  string
	TurnCount       int
	CompactionCount int
}

// ReplaceSessionData replaces all turns + compactions for one session_uuid
// in a single transaction. A full replace is the cleanest semantics given
// JSONL files are append-only in practice but we always re-ingest the whole
// file when the mtime changes.
func (s *Store) ReplaceSessionData(ctx context.Context, sp SessionPersist) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM turns WHERE session_uuid = ?`, sp.SessionUUID,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM compactions WHERE session_uuid = ?`, sp.SessionUUID,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM user_prompts WHERE session_uuid = ?`, sp.SessionUUID,
	); err != nil {
		return err
	}

	if len(sp.Turns) > 0 {
		ts, err := tx.PrepareContext(ctx, `
			INSERT INTO turns (
				session_uuid, turn_idx, ts, ts_unix_ms, model,
				input_tokens, output_tokens, cache_read,
				cache_create_5m, cache_create_1h,
				gap_s, classification, post_compact,
				project, source_path_hash,
				parent_session_uuid, cwd
			) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		`)
		if err != nil {
			return err
		}
		defer ts.Close()
		for _, t := range sp.Turns {
			pc := 0
			if t.PostCompact {
				pc = 1
			}
			if _, err := ts.ExecContext(ctx,
				t.SessionUUID, t.TurnIdx, t.TS, t.TSUnixMS, t.Model,
				t.InputTokens, t.OutputTokens, t.CacheRead,
				t.CacheCreate5m, t.CacheCreate1h,
				t.GapS, t.Classification, pc,
				t.Project, t.SourcePathHash,
				t.ParentSessionUUID, t.Cwd,
			); err != nil {
				return err
			}
		}
	}

	if len(sp.Compactions) > 0 {
		cs, err := tx.PrepareContext(ctx, `
			INSERT INTO compactions (
				session_uuid, ts, ts_unix_ms,
				prefix_tokens_est, summary_tokens_est,
				gap_to_prev_s, cache_state,
				confirmed, confirm_reason, project
			) VALUES (?,?,?,?,?,?,?,?,?,?)
		`)
		if err != nil {
			return err
		}
		defer cs.Close()
		for _, c := range sp.Compactions {
			conf := 0
			if c.Confirmed {
				conf = 1
			}
			var gap any
			if c.GapToPrevS != nil {
				gap = *c.GapToPrevS
			}
			if _, err := cs.ExecContext(ctx,
				c.SessionUUID, c.TS, c.TSUnixMS,
				c.PrefixTokensEst, c.SummaryTokensEst,
				gap, c.CacheState,
				conf, c.ConfirmReason, c.Project,
			); err != nil {
				return err
			}
		}
	}

	if len(sp.UserPrompts) > 0 {
		ps, err := tx.PrepareContext(ctx, `
			INSERT OR IGNORE INTO user_prompts (session_uuid, ts_unix_ms, text_preview)
			VALUES (?,?,?)
		`)
		if err != nil {
			return err
		}
		defer ps.Close()
		for _, p := range sp.UserPrompts {
			if _, err := ps.ExecContext(ctx, p.SessionUUID, p.TSUnixMS, p.TextPreview); err != nil {
				return err
			}
		}
	}

	return tx.Commit()
}

// GetIngestedMTime returns the mtime we last recorded for a given
// path_hash, or 0 if we've never ingested it.
func (s *Store) GetIngestedMTime(ctx context.Context, pathHash string) (int64, error) {
	var mt int64
	err := s.DB.QueryRowContext(ctx,
		`SELECT mtime_unix FROM ingested_files WHERE path_hash = ?`, pathHash,
	).Scan(&mt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return mt, nil
}

// RecordIngestedFile upserts the mtime tracking row.
func (s *Store) RecordIngestedFile(ctx context.Context, r IngestedFileRecord) error {
	if r.LastIngestedTS == "" {
		r.LastIngestedTS = time.Now().UTC().Format(time.RFC3339)
	}
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO ingested_files (path_hash, path, mtime_unix, last_ingested_ts, turn_count, compaction_count)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(path_hash) DO UPDATE SET
			path             = excluded.path,
			mtime_unix       = excluded.mtime_unix,
			last_ingested_ts = excluded.last_ingested_ts,
			turn_count       = excluded.turn_count,
			compaction_count = excluded.compaction_count
	`, r.PathHash, r.Path, r.MTimeUnix, r.LastIngestedTS, r.TurnCount, r.CompactionCount)
	return err
}
