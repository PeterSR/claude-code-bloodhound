package store

import (
	"context"
	"database/sql"
	"time"
)

// Reset and validity classification for the /usage percentage series.
//
// The rate limit windows are fixed-reset, not rolling: inside one window
// the reported percentage only ever rises. This was not an assumption — it
// was measured against the full observation history, where adjacent-poll
// drops came in exactly two shapes. Drops of exactly 1 point (296 of them)
// are integer rounding: /usage reports whole percents, and rounding can
// move a monotonically rising true value down by at most 1. Every larger
// drop was either a real reset or a single misparsed reading.
//
// That gives an exact rule with no tuned threshold. Walking a bucket's
// readings in time order and tracking the window's running peak:
//
//   - A reading at or above peak-1 continues the window (rounding jitter).
//   - A reading more than 1 below peak fell further than rounding allows.
//     If the next reading climbs back to the peak, the low one was a
//     misparse: mark it invalid and carry on. If it stays down, the window
//     rolled over: it's a reset, and a new window starts here.
//
// This replaced a fixed 30-point drop threshold that silently missed real
// resets (an 18%-to-0% rotation never tripped it) and had no concept of a
// misparse at all.
//
// A second, independent signal still applies: if a reading's timestamp has
// reached the reset time a previous reading advertised, a rotation happened
// even if the percentage didn't visibly crash — the daemon can be idle
// across the boundary and resume mid-window. That boundary heuristic is
// kept as-is.

// pctJitterPP is the largest drop integer rounding alone can produce
// between two readings of a monotonically rising true value. A drop of this
// size or less is noise around a flat-or-rising rate; a larger drop cannot
// come from rounding.
const pctJitterPP = 1

// pctReading is one bucket's reading in a time-ordered series, reduced to
// what classification needs.
type pctReading struct {
	TSUnixMS int64
	Pct      int
	HasPct   bool
	// ResetTS is the reset boundary this reading advertised, if any.
	ResetTS  time.Time
	HasReset bool
}

// pctFlags is the classification of one reading.
type pctFlags struct {
	ResetDetected bool
	// Valid is false for a misparse — a reading to be ignored downstream
	// rather than treated as either a real dip or a reset.
	Valid bool
}

// classifyBucket assigns reset and validity flags to a time-ordered series
// of one bucket's readings.
func classifyBucket(readings []pctReading) []pctFlags {
	out := make([]pctFlags, len(readings))

	var peak int
	var havePeak bool
	// prevReset is the most recent reset boundary a prior reading
	// advertised and that hasn't yet been crossed. Compared against the
	// current reading's own reset, never the reading itself.
	var prevReset time.Time
	var havePrevReset bool

	advanceReset := func(r pctReading, didReset bool) {
		switch {
		case r.HasReset:
			prevReset, havePrevReset = r.ResetTS, true
		case didReset:
			// Boundary consumed; don't let it re-trigger on later readings.
			havePrevReset = false
		}
	}

	for i := range readings {
		r := readings[i]
		out[i].Valid = true

		if !r.HasPct {
			advanceReset(r, false)
			continue
		}

		boundary := havePrevReset &&
			!time.UnixMilli(r.TSUnixMS).Before(prevReset)

		if !havePeak {
			out[i].ResetDetected = boundary
			peak, havePeak = r.Pct, true
			advanceReset(r, boundary)
			continue
		}

		switch {
		case boundary:
			out[i].ResetDetected = true
			peak = r.Pct
		case peak-r.Pct <= pctJitterPP:
			if r.Pct > peak {
				peak = r.Pct
			}
		case bucketRecovers(readings, i, peak):
			out[i].Valid = false // misparse; leave the peak intact
		default:
			out[i].ResetDetected = true
			peak = r.Pct
		}
		advanceReset(r, out[i].ResetDetected)
	}
	return out
}

// bucketRecovers reports whether the next reading with a value climbs back
// to the window's peak (within rounding). That is what separates a one-off
// misparse from a genuine reset.
func bucketRecovers(readings []pctReading, i, peak int) bool {
	for j := i + 1; j < len(readings); j++ {
		if !readings[j].HasPct {
			continue
		}
		return readings[j].Pct >= peak-pctJitterPP
	}
	// Nothing follows to disambiguate. Calling it a reset keeps the reading
	// usable as a window start; calling it a misparse would discard a real
	// observation on no evidence.
	return false
}

// execQuerier is the subset of *sql.DB / *sql.Tx that RecomputeUsageFlags
// needs, so the same pass runs both standalone (backfill) and inside
// RecordUsage's transaction (the new row included).
type execQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// RecomputeUsageFlags reclassifies session_reset_detected,
// week_reset_detected, session_pct_valid and week_pct_valid for every
// observation from the raw percentage history, writing back only the rows
// whose flags actually change.
//
// These flags are a pure function of the stored percentages, so this is
// safe to run anytime: it is the backfill for old rows, the continuous
// correction as new rows arrive, and a no-op once everything agrees.
func (s *Store) RecomputeUsageFlags(ctx context.Context) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := recomputeUsageFlags(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func recomputeUsageFlags(ctx context.Context, q execQuerier) error {
	rows, err := q.QueryContext(ctx, `
		SELECT id, ts_unix_ms,
		       session_pct, week_pct,
		       session_reset_ts, week_reset_ts,
		       session_reset_detected, week_reset_detected,
		       session_pct_valid, week_pct_valid
		FROM usage_observations
		ORDER BY ts_unix_ms ASC, id ASC
	`)
	if err != nil {
		return err
	}

	type stored struct {
		id                   int64
		sessReset, weekReset bool
		sessValid, weekValid bool
	}
	var (
		ids        []int64
		curr       []stored
		sess, week []pctReading
	)
	for rows.Next() {
		var (
			id                 int64
			tsMS               int64
			sPct, wPct         sql.NullInt64
			sResetTS, wResetTS sql.NullString
			sReset, wReset     int
			sValid, wValid     int
		)
		if err := rows.Scan(&id, &tsMS, &sPct, &wPct, &sResetTS, &wResetTS,
			&sReset, &wReset, &sValid, &wValid); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
		curr = append(curr, stored{
			id:        id,
			sessReset: sReset == 1, weekReset: wReset == 1,
			sessValid: sValid == 1, weekValid: wValid == 1,
		})
		sess = append(sess, toReading(tsMS, sPct, sResetTS))
		week = append(week, toReading(tsMS, wPct, wResetTS))
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	sessFlags := classifyBucket(sess)
	weekFlags := classifyBucket(week)

	for i, id := range ids {
		want := stored{
			id:        id,
			sessReset: sessFlags[i].ResetDetected, weekReset: weekFlags[i].ResetDetected,
			sessValid: sessFlags[i].Valid, weekValid: weekFlags[i].Valid,
		}
		if want == curr[i] {
			continue
		}
		if _, err := q.ExecContext(ctx, `
			UPDATE usage_observations
			SET session_reset_detected = ?, week_reset_detected = ?,
			    session_pct_valid = ?, week_pct_valid = ?
			WHERE id = ?
		`, b2i(want.sessReset), b2i(want.weekReset),
			b2i(want.sessValid), b2i(want.weekValid), id); err != nil {
			return err
		}
	}
	return nil
}

func toReading(tsMS int64, pct sql.NullInt64, resetTS sql.NullString) pctReading {
	r := pctReading{TSUnixMS: tsMS}
	if pct.Valid {
		r.Pct, r.HasPct = int(pct.Int64), true
	}
	if resetTS.Valid {
		if t, err := time.Parse(time.RFC3339, resetTS.String); err == nil {
			r.ResetTS, r.HasReset = t, true
		}
	}
	return r
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
