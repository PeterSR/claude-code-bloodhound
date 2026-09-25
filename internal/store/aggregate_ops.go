package store

import (
	"context"
)

// SessionRow mirrors the `sessions` table.
type SessionRow struct {
	SessionUUID         string
	Project             string
	FirstTSUnixMS       int64
	LastTSUnixMS        int64
	TurnCount           int
	RawTokens           int64
	OutputTokens        int64
	Peak5hRawTokens     int64
	IdleMissCount       int
	RotationCount       int
	RestructureCount    int
	CompactionCount     int
	ColdCompactionCount int
	CacheTTL            string
	Models              string // comma-separated
	// ParentSessionUUID is "" for a normal top-level session, otherwise the
	// session it was dispatched as a subagent of.
	ParentSessionUUID string
	// Cwd is the session's own working directory (see turns.cwd for why
	// this can't be reconstructed from Project alone).
	Cwd string
	// AccountID is the account of the session's latest turn.
	AccountID int64
}

// BucketRow mirrors the `buckets` table.
type BucketRow struct {
	StartUnixMS       int64
	EndUnixMS         int64
	ResetInferred     bool
	RawTokenTotal     int64
	CostWeightedTotal float64
	OutputTokenTotal  int64
	TurnCount         int
}

// ReplaceSessions wipes and re-inserts the entire sessions table in one tx.
func (s *Store) ReplaceSessions(ctx context.Context, rows []SessionRow) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions`); err != nil {
		return err
	}
	if len(rows) == 0 {
		return tx.Commit()
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO sessions (
			session_uuid, project,
			first_ts_unix_ms, last_ts_unix_ms,
			turn_count, raw_tokens, output_tokens, peak_5h_raw_tokens,
			idle_miss_count, rotation_count, restructure_count,
			compaction_count, cold_compaction_count,
			cache_ttl, models,
			parent_session_uuid, cwd, account_id
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx,
			r.SessionUUID, r.Project,
			r.FirstTSUnixMS, r.LastTSUnixMS,
			r.TurnCount, r.RawTokens, r.OutputTokens, r.Peak5hRawTokens,
			r.IdleMissCount, r.RotationCount, r.RestructureCount,
			r.CompactionCount, r.ColdCompactionCount,
			r.CacheTTL, r.Models,
			r.ParentSessionUUID, r.Cwd, accountOr1(r.AccountID),
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReplaceBuckets wipes and re-inserts one account's buckets in one tx.
func (s *Store) ReplaceBuckets(ctx context.Context, accountID int64, rows []BucketRow) error {
	accountID = accountOr1(accountID)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM buckets WHERE account_id = ?`, accountID); err != nil {
		return err
	}
	if len(rows) == 0 {
		return tx.Commit()
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO buckets (
			account_id, start_unix_ms, end_unix_ms, reset_inferred,
			raw_token_total, cost_weighted_total, output_token_total, turn_count
		) VALUES (?,?,?,?,?,?,?,?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range rows {
		ri := 0
		if r.ResetInferred {
			ri = 1
		}
		if _, err := stmt.ExecContext(ctx,
			accountID, r.StartUnixMS, r.EndUnixMS, ri,
			r.RawTokenTotal, r.CostWeightedTotal, r.OutputTokenTotal, r.TurnCount,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}
