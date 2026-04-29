package store

import (
	"context"
	"database/sql"
	"errors"
)

// LatestObservation is the most recent /usage scrape's persisted form.
type LatestObservation struct {
	ID                   int64
	TSUnixMS             int64
	TSISO                string
	SessionPct           *int
	WeekPct              *int
	SessionResetRaw      string
	WeekResetRaw         string
	SessionResetTSISO    string // RFC3339, or "" if unparsed
	WeekResetTSISO       string
	SessionResetDetected bool
	WeekResetDetected    bool
	ElapsedS             float64
	ParseOK              bool
}

// LatestUsage returns the most recent observation, or nil + nil error if
// none exist.
func (s *Store) LatestUsage(ctx context.Context) (*LatestObservation, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT id, ts, ts_unix_ms,
		       session_pct, week_pct,
		       session_reset_raw, week_reset_raw,
		       session_reset_ts, week_reset_ts,
		       session_reset_detected, week_reset_detected,
		       elapsed_s, parse_ok
		FROM usage_observations
		ORDER BY ts_unix_ms DESC LIMIT 1
	`)
	var (
		o                                       LatestObservation
		sessPct, weekPct                        sql.NullInt64
		sessRaw, weekRaw, sessTS, weekTS        sql.NullString
		sessReset, weekReset, parseOK           int
		elapsed                                 sql.NullFloat64
	)
	err := row.Scan(&o.ID, &o.TSISO, &o.TSUnixMS,
		&sessPct, &weekPct,
		&sessRaw, &weekRaw,
		&sessTS, &weekTS,
		&sessReset, &weekReset,
		&elapsed, &parseOK,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if sessPct.Valid {
		v := int(sessPct.Int64)
		o.SessionPct = &v
	}
	if weekPct.Valid {
		v := int(weekPct.Int64)
		o.WeekPct = &v
	}
	if sessRaw.Valid {
		o.SessionResetRaw = sessRaw.String
	}
	if weekRaw.Valid {
		o.WeekResetRaw = weekRaw.String
	}
	if sessTS.Valid {
		o.SessionResetTSISO = sessTS.String
	}
	if weekTS.Valid {
		o.WeekResetTSISO = weekTS.String
	}
	o.SessionResetDetected = sessReset == 1
	o.WeekResetDetected = weekReset == 1
	if elapsed.Valid {
		o.ElapsedS = elapsed.Float64
	}
	o.ParseOK = parseOK == 1
	return &o, nil
}

// PctPoint is one (timestamp, percentage) sample for burn-rate projection.
type PctPoint struct {
	TSUnixMS int64
	Pct      int
}

// SessionPctSinceLastReset returns session %s since the most recent
// session_reset_detected, ordered by time. Used for burn-rate projection.
func (s *Store) SessionPctSinceLastReset(ctx context.Context) ([]PctPoint, error) {
	return s.pctSince(ctx, "session_pct", "session_reset_detected")
}

// WeekPctSinceLastReset is the week analog of SessionPctSinceLastReset.
func (s *Store) WeekPctSinceLastReset(ctx context.Context) ([]PctPoint, error) {
	return s.pctSince(ctx, "week_pct", "week_reset_detected")
}

func (s *Store) pctSince(ctx context.Context, pctCol, resetCol string) ([]PctPoint, error) {
	q := `
		SELECT ts_unix_ms, ` + pctCol + `
		FROM usage_observations
		WHERE ` + pctCol + ` IS NOT NULL
		  AND ts_unix_ms > COALESCE(
		       (SELECT MAX(ts_unix_ms) FROM usage_observations WHERE ` + resetCol + ` = 1),
		       0)
		ORDER BY ts_unix_ms ASC
	`
	rows, err := s.DB.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PctPoint
	for rows.Next() {
		var p PctPoint
		if err := rows.Scan(&p.TSUnixMS, &p.Pct); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// LatestRawDump returns the tail of the most recent /usage TUI capture.
func (s *Store) LatestRawDump(ctx context.Context) (string, int64, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT payload, ts_unix_ms FROM raw_dumps
		ORDER BY id DESC LIMIT 1
	`)
	var payload string
	var ts int64
	err := row.Scan(&payload, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil
	}
	return payload, ts, err
}

// IngestSummary is a coarse-grained snapshot of ingester output.
type IngestSummary struct {
	FilesTracked     int
	TurnsTotal       int
	CompactionsTotal int
	LatestTurnTSMS   int64
}

// IngestStats returns a quick coarse summary of what the ingester has
// produced. Cheap (three COUNT/MAX queries).
func (s *Store) IngestStats(ctx context.Context) (IngestSummary, error) {
	var sum IngestSummary
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ingested_files`,
	).Scan(&sum.FilesTracked); err != nil {
		return sum, err
	}
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM turns`,
	).Scan(&sum.TurnsTotal); err != nil {
		return sum, err
	}
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM compactions WHERE confirmed = 1`,
	).Scan(&sum.CompactionsTotal); err != nil {
		return sum, err
	}
	var latest sql.NullInt64
	if err := s.DB.QueryRowContext(ctx,
		`SELECT MAX(ts_unix_ms) FROM turns`,
	).Scan(&latest); err != nil {
		return sum, err
	}
	if latest.Valid {
		sum.LatestTurnTSMS = latest.Int64
	}
	return sum, nil
}

// SessionListRow is the trimmed-for-list view of `sessions`.
type SessionListRow struct {
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
	Models              string
}

// ListSessions returns every persisted session summary, ordered by most
// recent activity first.
func (s *Store) ListSessions(ctx context.Context) ([]SessionListRow, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT session_uuid, project,
		       first_ts_unix_ms, last_ts_unix_ms,
		       turn_count, raw_tokens, output_tokens, peak_5h_raw_tokens,
		       idle_miss_count, rotation_count, restructure_count,
		       compaction_count, cold_compaction_count,
		       cache_ttl, models
		FROM sessions
		ORDER BY last_ts_unix_ms DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionListRow
	for rows.Next() {
		var r SessionListRow
		if err := rows.Scan(
			&r.SessionUUID, &r.Project,
			&r.FirstTSUnixMS, &r.LastTSUnixMS,
			&r.TurnCount, &r.RawTokens, &r.OutputTokens, &r.Peak5hRawTokens,
			&r.IdleMissCount, &r.RotationCount, &r.RestructureCount,
			&r.CompactionCount, &r.ColdCompactionCount,
			&r.CacheTTL, &r.Models,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LatestBucket returns the most recent 5h bucket, or nil if none.
func (s *Store) LatestBucket(ctx context.Context) (*BucketRow, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT start_unix_ms, end_unix_ms, reset_inferred,
		       raw_token_total, cost_weighted_total, output_token_total, turn_count
		FROM buckets
		ORDER BY start_unix_ms DESC LIMIT 1
	`)
	var r BucketRow
	var ri int
	err := row.Scan(&r.StartUnixMS, &r.EndUnixMS, &ri,
		&r.RawTokenTotal, &r.CostWeightedTotal, &r.OutputTokenTotal, &r.TurnCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.ResetInferred = ri == 1
	return &r, nil
}
