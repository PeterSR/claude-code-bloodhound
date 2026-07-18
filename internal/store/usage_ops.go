package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/usage"
)

// SaturatedThresholdPct is the percentage at or above which a bucket is
// treated as saturated (hit cap). Used by RecordUsage to tag observations
// so future tokens-per-1% calibration can skip them — when pct stops
// moving but tokens continue accumulating (e.g. on Anthropic's on-demand
// "Extra usage" tier), the naive Δtokens/Δpct ratio explodes.
const SaturatedThresholdPct = 99

// Observation is a tiny summary of what RecordUsage just persisted.
type Observation struct {
	ID                   int64
	TS                   time.Time
	SessionPct           *int
	WeekPct              *int
	ParseOK              bool
	SessionResetDetected bool
	WeekResetDetected    bool
	SessionSaturated     bool
	WeekSaturated        bool
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

	// The Result already carries the extracted values directly.
	var sessionPct, weekPct sql.NullInt64
	var sessionResetRaw, weekResetRaw sql.NullString
	if res.SessionPct != nil {
		sessionPct = sql.NullInt64{Int64: int64(*res.SessionPct), Valid: true}
	}
	if res.WeekPct != nil {
		weekPct = sql.NullInt64{Int64: int64(*res.WeekPct), Valid: true}
	}
	if res.SessionResetRaw != "" {
		sessionResetRaw = sql.NullString{String: res.SessionResetRaw, Valid: true}
	}
	if res.WeekResetRaw != "" {
		weekResetRaw = sql.NullString{String: res.WeekResetRaw, Valid: true}
	}

	// Parse reset hints (best-effort). Pass the IANA timezone captured
	// from the panel so wall-clock times resolve to the correct UTC
	// instant — the TUI emits the time and zone separately.
	var sessionResetTS, weekResetTS sql.NullString
	if sessionResetRaw.Valid {
		if t, ok := usage.ParseReset(sessionResetRaw.String, res.SessionResetTZ, res.FetchedAt); ok {
			sessionResetTS = sql.NullString{String: t.UTC().Format(time.RFC3339), Valid: true}
		}
	}
	if weekResetRaw.Valid {
		if t, ok := usage.ParseReset(weekResetRaw.String, res.WeekResetTZ, res.FetchedAt); ok {
			weekResetTS = sql.NullString{String: t.UTC().Format(time.RFC3339), Valid: true}
		}
	}

	// Reset detection and misparse flagging are derived from the whole
	// percentage series, not from this one row against its predecessor: a
	// misparse can only be told from a real drop by whether the *next*
	// reading recovers. So the new row goes in with placeholder flags, then
	// RecomputeUsageFlags reclassifies the tail (and self-heals any older
	// rows whose classification the new data changes). See usage_flags.go.
	parseOK := 0
	if res.OK {
		parseOK = 1
	}

	sessionSaturated := 0
	weekSaturated := 0
	if sessionPct.Valid && sessionPct.Int64 >= SaturatedThresholdPct {
		sessionSaturated = 1
	}
	if weekPct.Valid && weekPct.Int64 >= SaturatedThresholdPct {
		weekSaturated = 1
	}

	// Reset/validity flags start as placeholders (no reset, valid); the
	// recompute below sets them from the series.
	r, err := tx.ExecContext(ctx,
		`INSERT INTO usage_observations (
			ts, ts_unix_ms,
			session_pct, week_pct,
			session_reset_raw, week_reset_raw,
			session_reset_ts, week_reset_ts,
			raw_dump_id,
			session_reset_detected, week_reset_detected,
			session_saturated, week_saturated,
			session_pct_valid, week_pct_valid,
			elapsed_s, parse_ok
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?, 1, 1, ?, ?)`,
		tsISO, tsMS,
		nullInt(sessionPct), nullInt(weekPct),
		nullStr(sessionResetRaw), nullStr(weekResetRaw),
		nullStr(sessionResetTS), nullStr(weekResetTS),
		dumpID,
		sessionSaturated, weekSaturated,
		res.ElapsedS, parseOK,
	)
	if err != nil {
		return Observation{}, err
	}
	id, _ := r.LastInsertId()

	// Classify the new row (and correct any older rows the new reading
	// disambiguates) before committing, so readers never see the
	// placeholder flags.
	if err := recomputeUsageFlags(ctx, tx); err != nil {
		return Observation{}, err
	}

	var sessReset, weekReset int
	if err := tx.QueryRowContext(ctx,
		`SELECT session_reset_detected, week_reset_detected
		   FROM usage_observations WHERE id = ?`, id,
	).Scan(&sessReset, &weekReset); err != nil {
		return Observation{}, err
	}

	if err := tx.Commit(); err != nil {
		return Observation{}, err
	}

	out := Observation{
		ID:                   id,
		TS:                   res.FetchedAt,
		ParseOK:              res.OK,
		SessionResetDetected: sessReset == 1,
		WeekResetDetected:    weekReset == 1,
		SessionSaturated:     sessionSaturated == 1,
		WeekSaturated:        weekSaturated == 1,
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
