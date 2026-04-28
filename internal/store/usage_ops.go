package store

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/usage"
)

// Observation is a tiny summary of what RecordUsage just persisted.
type Observation struct {
	ID                   int64
	TS                   time.Time
	SessionPct           *int
	WeekPct              *int
	ParseOK              bool
	SessionResetDetected bool
	WeekResetDetected    bool
}

// RecordUsage persists a /usage scrape (success or failure) into raw_dumps +
// usage_observations, computes simple reset detection against the prior
// observation, and returns a summary of what landed.
func (s *Store) RecordUsage(ctx context.Context, res usage.Result, fetchErr error) (Observation, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Observation{}, err
	}
	defer func() { _ = tx.Rollback() }()

	tsMS := res.FetchedAt.UnixMilli()
	tsISO := res.FetchedAt.UTC().Format(time.RFC3339Nano)

	// raw_dump
	rd, err := tx.ExecContext(ctx,
		`INSERT INTO raw_dumps (ts_unix_ms, payload) VALUES (?, ?)`,
		tsMS, res.Raw,
	)
	if err != nil {
		return Observation{}, err
	}
	dumpID, _ := rd.LastInsertId()

	// Pull session + week buckets from result (the package already chose them).
	var sessionPct, weekPct sql.NullInt64
	var sessionResetRaw, weekResetRaw sql.NullString
	for _, b := range res.Buckets {
		l := strings.ToLower(b.Label)
		if !sessionPct.Valid && strings.Contains(l, "session") {
			sessionPct = sql.NullInt64{Int64: int64(b.Pct), Valid: true}
			if b.ResetRaw != "" {
				sessionResetRaw = sql.NullString{String: b.ResetRaw, Valid: true}
			}
		}
		if !weekPct.Valid && strings.Contains(l, "week") &&
			(strings.Contains(l, "all") || !strings.Contains(l, "(")) {
			weekPct = sql.NullInt64{Int64: int64(b.Pct), Valid: true}
			if b.ResetRaw != "" {
				weekResetRaw = sql.NullString{String: b.ResetRaw, Valid: true}
			}
		}
	}

	// Parse reset hints (best-effort).
	var sessionResetTS, weekResetTS sql.NullString
	if sessionResetRaw.Valid {
		if t, ok := usage.ParseReset(sessionResetRaw.String, res.FetchedAt); ok {
			sessionResetTS = sql.NullString{String: t.UTC().Format(time.RFC3339), Valid: true}
		}
	}
	if weekResetRaw.Valid {
		if t, ok := usage.ParseReset(weekResetRaw.String, res.FetchedAt); ok {
			weekResetTS = sql.NullString{String: t.UTC().Format(time.RFC3339), Valid: true}
		}
	}

	// Reset detection: compare to most-recent prior observation's percentages.
	var prevSessionPct, prevWeekPct sql.NullInt64
	row := tx.QueryRowContext(ctx,
		`SELECT session_pct, week_pct FROM usage_observations ORDER BY ts_unix_ms DESC LIMIT 1`,
	)
	_ = row.Scan(&prevSessionPct, &prevWeekPct)
	sessionResetDetected := 0
	weekResetDetected := 0
	if prevSessionPct.Valid && sessionPct.Valid && sessionPct.Int64 < prevSessionPct.Int64 {
		sessionResetDetected = 1
	}
	if prevWeekPct.Valid && weekPct.Valid && weekPct.Int64 < prevWeekPct.Int64 {
		weekResetDetected = 1
	}

	parseOK := 0
	if res.OK {
		parseOK = 1
	}

	r, err := tx.ExecContext(ctx,
		`INSERT INTO usage_observations (
			ts, ts_unix_ms,
			session_pct, week_pct,
			session_reset_raw, week_reset_raw,
			session_reset_ts, week_reset_ts,
			raw_dump_id,
			session_reset_detected, week_reset_detected,
			elapsed_s, parse_ok
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		tsISO, tsMS,
		nullInt(sessionPct), nullInt(weekPct),
		nullStr(sessionResetRaw), nullStr(weekResetRaw),
		nullStr(sessionResetTS), nullStr(weekResetTS),
		dumpID,
		sessionResetDetected, weekResetDetected,
		res.ElapsedS, parseOK,
	)
	if err != nil {
		return Observation{}, err
	}
	id, _ := r.LastInsertId()

	if err := tx.Commit(); err != nil {
		return Observation{}, err
	}

	out := Observation{
		ID:                   id,
		TS:                   res.FetchedAt,
		ParseOK:              res.OK,
		SessionResetDetected: sessionResetDetected == 1,
		WeekResetDetected:    weekResetDetected == 1,
	}
	if sessionPct.Valid {
		v := int(sessionPct.Int64)
		out.SessionPct = &v
	}
	if weekPct.Valid {
		v := int(weekPct.Int64)
		out.WeekPct = &v
	}
	return out, nil
}

func nullInt(v sql.NullInt64) any {
	if v.Valid {
		return v.Int64
	}
	return nil
}

func nullStr(v sql.NullString) any {
	if v.Valid {
		return v.String
	}
	return nil
}
