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
	SessionSaturated     bool
	WeekSaturated        bool
	ElapsedS             float64
	ParseOK              bool
	Source               string // "api" | "pty"
}

// observationCols is the column list both observation queries select, in
// the order scanObservation reads them. Shared so the two can't drift.
const observationCols = `
	SELECT id, ts, ts_unix_ms,
	       session_pct, week_pct,
	       session_reset_raw, week_reset_raw,
	       session_reset_ts, week_reset_ts,
	       session_reset_detected, week_reset_detected,
	       session_saturated, week_saturated,
	       elapsed_s, parse_ok, source
	FROM usage_observations
`

// LatestUsage returns the most recent observation, or nil + nil error if
// none exist. It does not filter on parse_ok: callers that need freshness
// (how long since we last talked to claude at all) want the failed
// attempts too. Callers that need a reading use LatestParsedUsage.
func (s *Store) LatestUsage(ctx context.Context, accountID int64) (*LatestObservation, error) {
	return scanObservation(s.DB.QueryRowContext(ctx,
		observationCols+`WHERE account_id = ? ORDER BY ts_unix_ms DESC LIMIT 1`, accountOr1(accountID)))
}

// LatestParsedUsage returns the most recent observation that actually
// extracted, or nil + nil error if none ever has.
//
// This is the fallback that keeps a single failed poll from blanking the
// gauges. A poll can come up empty for reasons that say nothing about the
// numbers being wrong (the panel was still loading when the capture
// deadline hit, most commonly), and on a 5-minute poll interval that would
// otherwise leave every consumer with no percentage at all until the next
// cycle. The previous reading is still the best answer available; it is
// just older than the poll timestamp suggests, which is what the Stale
// flag on the computed window exists to say.
func (s *Store) LatestParsedUsage(ctx context.Context, accountID int64) (*LatestObservation, error) {
	return scanObservation(s.DB.QueryRowContext(ctx,
		observationCols+`WHERE account_id = ? AND parse_ok = 1 ORDER BY ts_unix_ms DESC LIMIT 1`, accountOr1(accountID)))
}

func scanObservation(row *sql.Row) (*LatestObservation, error) {
	var (
		o                                LatestObservation
		sessPct, weekPct                 sql.NullInt64
		sessRaw, weekRaw, sessTS, weekTS sql.NullString
		sessReset, weekReset, parseOK    int
		sessSat, weekSat                 int
		elapsed                          sql.NullFloat64
	)
	err := row.Scan(&o.ID, &o.TSISO, &o.TSUnixMS,
		&sessPct, &weekPct,
		&sessRaw, &weekRaw,
		&sessTS, &weekTS,
		&sessReset, &weekReset,
		&sessSat, &weekSat,
		&elapsed, &parseOK, &o.Source,
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
	o.SessionSaturated = sessSat == 1
	o.WeekSaturated = weekSat == 1
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
func (s *Store) SessionPctSinceLastReset(ctx context.Context, accountID int64) ([]PctPoint, error) {
	return s.pctSince(ctx, accountID, "session_pct", "session_pct_valid", "session_saturated", "session_reset_detected")
}

// WeekPctSinceLastReset is the week analog of SessionPctSinceLastReset.
func (s *Store) WeekPctSinceLastReset(ctx context.Context, accountID int64) ([]PctPoint, error) {
	return s.pctSince(ctx, accountID, "week_pct", "week_pct_valid", "week_saturated", "week_reset_detected")
}

// pctSince feeds slopeOver (burn.go) and etaToLimit (the statusline): an
// endpoint-to-endpoint slope has no chance to notice a bad endpoint the way
// a smoothed series can, so a single flagged misparse or saturated reading
// at either end of the window would otherwise silently poison the rate.
// The valid/saturated filters mirror the guards burnSeries already applies
// to the same two columns.
//
// ts_unix_ms >= (not >) the last reset: a reset is detected AT a reading,
// which is also the new window's first point. Excluding it with a strict
// `>` discarded that point, so every window projected a rate over one fewer
// observation than it actually had.
func (s *Store) pctSince(ctx context.Context, accountID int64, pctCol, validCol, saturatedCol, resetCol string) ([]PctPoint, error) {
	q := `
		SELECT ts_unix_ms, ` + pctCol + `
		FROM usage_observations
		WHERE account_id = ?1
		  AND ` + pctCol + ` IS NOT NULL
		  AND ` + validCol + ` = 1
		  AND ` + saturatedCol + ` = 0
		  AND ts_unix_ms >= COALESCE(
		       (SELECT MAX(ts_unix_ms) FROM usage_observations WHERE account_id = ?1 AND ` + resetCol + ` = 1),
		       0)
		ORDER BY ts_unix_ms ASC
	`
	rows, err := s.DB.QueryContext(ctx, q, accountOr1(accountID))
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
	// SubagentCount and SubagentTurnCount summarize the Task-tool sessions
	// this one dispatched (sessions whose parent_session_uuid points back
	// here). A subagent is an implementation detail of the session that
	// dispatched it and its own row is excluded from ListSessions below, so
	// these are how the list still shows that delegation happened without
	// flooding the table with the subagents themselves.
	SubagentCount     int
	SubagentTurnCount int
}

// ListSessions returns every top-level session summary, ordered by most
// recent activity first. Subagent (Task-tool) sessions are excluded: the
// parent that dispatched them already carries their cost via the
// effective-owner rollup in SessionPctTotalsAll, and a supervisor session
// that fans out ten subagents would otherwise show up as eleven rows, ten
// of them near-empty. Use SubagentCount / SubagentTurnCount to see that a
// session delegated work, and the session-detail payload to see what it
// dispatched.
// ListSessions lists one account's top-level sessions, newest first.
func (s *Store) ListSessions(ctx context.Context, accountID int64) ([]SessionListRow, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT s.session_uuid, s.project,
		       s.first_ts_unix_ms, s.last_ts_unix_ms,
		       s.turn_count, s.raw_tokens, s.output_tokens, s.peak_5h_raw_tokens,
		       s.idle_miss_count, s.rotation_count, s.restructure_count,
		       s.compaction_count, s.cold_compaction_count,
		       s.cache_ttl, s.models,
		       COALESCE(sub.subagent_count, 0), COALESCE(sub.subagent_turns, 0)
		FROM sessions s
		LEFT JOIN (
			SELECT parent_session_uuid AS puuid,
			       COUNT(*)           AS subagent_count,
			       SUM(turn_count)     AS subagent_turns
			FROM sessions
			WHERE parent_session_uuid <> ''
			GROUP BY parent_session_uuid
		) sub ON sub.puuid = s.session_uuid
		WHERE s.parent_session_uuid = '' AND s.account_id = ?
		ORDER BY s.last_ts_unix_ms DESC
	`, accountOr1(accountID))
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
			&r.SubagentCount, &r.SubagentTurnCount,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LatestBucket returns the most recent 5h bucket, or nil if none.
func (s *Store) LatestBucket(ctx context.Context, accountID int64) (*BucketRow, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT start_unix_ms, end_unix_ms, reset_inferred,
		       raw_token_total, cost_weighted_total, output_token_total, turn_count
		FROM buckets
		WHERE account_id = ?
		ORDER BY start_unix_ms DESC LIMIT 1
	`, accountOr1(accountID))
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

// ModelsWithSpend returns the distinct models that have actually consumed
// tokens, newest-active first. Models with no token spend (the
// "<synthetic>" pseudo-model the ingester writes for API-error messages,
// which is all zeros) are excluded, so price discovery never chases a name
// that was never a real model.
func (s *Store) ModelsWithSpend(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT model
		FROM turns
		GROUP BY model
		HAVING SUM(input_tokens + output_tokens + cache_read +
		           cache_create_5m + cache_create_1h) > 0
		ORDER BY MAX(ts_unix_ms) DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
