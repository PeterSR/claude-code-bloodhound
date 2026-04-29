package aggregate

import (
	"context"
	"database/sql"

	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

// refreshCalibration rebuilds calibration_points from usage_observations +
// turns. For each bucket (session, week) we walk observations in time
// order and emit one point per adjacent pair (a, b) where:
//
//   - both have a non-NULL pct for that bucket,
//   - neither is saturated for that bucket (saturation distorts Δtokens/Δpct
//     toward infinity once the bucket caps),
//   - no reset was detected at b (a reset between a and b means the pcts
//     are not directly comparable — pct rolled back to 0),
//   - b.pct > a.pct (we only learn from positive moves; flat or negative
//     moves are noise from the rolling window).
//
// Tokens between a and b come from the `turns` table; we sum cost-weighted
// (the same formula buckets uses) and raw tokens, in the half-open window
// (a.ts, b.ts].
func refreshCalibration(ctx context.Context, s *store.Store) (int, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM calibration_points`); err != nil {
		return 0, err
	}

	type obs struct {
		id        int64
		tsMS      int64
		sessPct   sql.NullInt64
		weekPct   sql.NullInt64
		sessSat   bool
		weekSat   bool
		sessReset bool
		weekReset bool
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT id, ts_unix_ms,
		       session_pct, week_pct,
		       session_saturated, week_saturated,
		       session_reset_detected, week_reset_detected
		FROM usage_observations
		ORDER BY ts_unix_ms ASC, id ASC
	`)
	if err != nil {
		return 0, err
	}
	var observations []obs
	for rows.Next() {
		var o obs
		var ss, ws, sr, wr int
		if err := rows.Scan(&o.id, &o.tsMS, &o.sessPct, &o.weekPct, &ss, &ws, &sr, &wr); err != nil {
			rows.Close()
			return 0, err
		}
		o.sessSat = ss == 1
		o.weekSat = ws == 1
		o.sessReset = sr == 1
		o.weekReset = wr == 1
		observations = append(observations, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	if len(observations) < 2 {
		return 0, tx.Commit()
	}

	// Pre-load all turns once. Even at 100k turns this is cheap (≈ 16MB)
	// and we save ourselves a per-pair query.
	type turn struct {
		tsMS int64
		raw  int64
		out  int64
		cw   float64
	}
	trows, err := tx.QueryContext(ctx, `
		SELECT ts_unix_ms,
		       input_tokens, output_tokens, cache_read,
		       cache_create_5m, cache_create_1h
		FROM turns
		ORDER BY ts_unix_ms ASC
	`)
	if err != nil {
		return 0, err
	}
	var turns []turn
	for trows.Next() {
		var (
			tsMS                    int64
			in, out, cr, cw5m, cw1h int64
		)
		if err := trows.Scan(&tsMS, &in, &out, &cr, &cw5m, &cw1h); err != nil {
			trows.Close()
			return 0, err
		}
		raw := in + out + cr + cw5m + cw1h
		cw := float64(cr)*0.1 + float64(cw5m)*1.25 + float64(cw1h)*2.0 +
			float64(in)*1.0 + float64(out)*5.0
		turns = append(turns, turn{tsMS: tsMS, raw: raw, out: out, cw: cw})
	}
	trows.Close()
	if err := trows.Err(); err != nil {
		return 0, err
	}

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO calibration_points (
			bucket, a_obs_id, b_obs_id,
			a_ts_unix_ms, b_ts_unix_ms,
			a_pct, b_pct, delta_pct,
			raw_tokens, cost_weighted_tokens, output_tokens, turn_count,
			gap_s,
			tokens_per_pct_raw, tokens_per_pct_cw
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	count := 0
	emit := func(bucket string, a, b obs, aPct, bPct int64) error {
		delta := bPct - aPct
		if delta <= 0 {
			return nil
		}
		var raw, outTok int64
		var cw float64
		var turnCount int
		// Half-open window (a.ts, b.ts]; turns is sorted so we could
		// binary-search, but a linear scan over a few thousand rows
		// per emit is fine for now.
		for _, t := range turns {
			if t.tsMS <= a.tsMS {
				continue
			}
			if t.tsMS > b.tsMS {
				break
			}
			raw += t.raw
			outTok += t.out
			cw += t.cw
			turnCount++
		}
		if raw == 0 {
			// No turns in the window — likely the user's /usage updated
			// from accounting drift, not new spend. Skip.
			return nil
		}
		gap := float64(b.tsMS-a.tsMS) / 1000.0
		_, err := stmt.ExecContext(ctx,
			bucket, a.id, b.id,
			a.tsMS, b.tsMS,
			aPct, bPct, delta,
			raw, cw, outTok, turnCount,
			gap,
			float64(raw)/float64(delta),
			cw/float64(delta),
		)
		if err != nil {
			return err
		}
		count++
		return nil
	}

	// Walk for session.
	for i := 1; i < len(observations); i++ {
		a, b := observations[i-1], observations[i]
		if !a.sessPct.Valid || !b.sessPct.Valid {
			continue
		}
		if a.sessSat || b.sessSat {
			continue
		}
		if b.sessReset {
			continue
		}
		if err := emit("session", a, b, a.sessPct.Int64, b.sessPct.Int64); err != nil {
			return 0, err
		}
	}

	// Walk for week.
	for i := 1; i < len(observations); i++ {
		a, b := observations[i-1], observations[i]
		if !a.weekPct.Valid || !b.weekPct.Valid {
			continue
		}
		if a.weekSat || b.weekSat {
			continue
		}
		if b.weekReset {
			continue
		}
		if err := emit("week", a, b, a.weekPct.Int64, b.weekPct.Int64); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}
