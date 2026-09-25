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
//
// Two refinements on top of the exact rule above, both found by auditing a
// live snapshot:
//
//   - A reading above 100 is rejected outright, before it ever reaches the
//     peak-tracking logic. /usage reports a percentage of a hard cap, so
//     anything over 100 is a parse error, not a data point (21 such rows
//     on the audited snapshot, e.g. a 109 where the panel showed 100).
//     Rejecting rather than clamping to 100 matters: an impossible reading
//     tells us nothing about what the true value actually was, only that it
//     wasn't the number reported, so clamping would hand the peak a value
//     we invented just as surely as the misparse did. Concretely, clamping
//     the 109 to 100 could still ratchet the peak up past the honest
//     reading that follows it, which is the exact failure this whole
//     scheme replaced (see the 18%-to-0% history above). Treating it like
//     a parse gap that also happens to be marked invalid (not merely
//     absent) sidesteps that: it can't move the peak or trip a reset, and
//     downstream consumers can still tell "no reading" from "reading
//     rejected."
//
//   - bucketRecovers looks past a run of readings that stay within
//     rounding of the initial dip, not just the single next one. The
//     week bucket in particular polls faster than its displayed integer
//     ticks, so a jitter dip such as 30/28/28/28/30 repeats the low value
//     for several polls before climbing back: the old one-reading
//     lookahead saw only another 28 and called it a persistent drop,
//     flagging 28 reset points for roughly 13 real weekly resets. Scanning
//     past the plateau (stopping the moment a reading breaks it, either by
//     climbing back to the peak or by drifting to a genuinely new low)
//     recovers the exact rule without adding a tuned lookahead distance.

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

		if r.Pct > 100 {
			// Physically impossible; see the header comment for why this is
			// rejected rather than clamped. Treated like a parse gap for
			// peak-tracking purposes, but marked invalid rather than absent.
			out[i].Valid = false
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

// bucketRecovers reports whether the reading run following the drop at i
// climbs back to the window's peak (within rounding). That is what
// separates a one-off misparse from a genuine reset, but the same jitter
// that causes a one-poll misparse can also repeat the identical low value
// for several polls in a row before ticking back up (the true value moves
// far slower than the poll interval). So: skip past a run of readings that
// stay within rounding of the initial dip, and only decide once one of them
// breaks that plateau, either by climbing back to the peak (misparse) or by
// drifting to a new, lower value (a genuine, if noisy, decline).
func bucketRecovers(readings []pctReading, i, peak int) bool {
	low := readings[i].Pct
	for j := i + 1; j < len(readings); j++ {
		if !readings[j].HasPct {
			continue
		}
		v := readings[j].Pct
		if v >= low-pctJitterPP && v <= low+pctJitterPP {
			// Still the same low plateau as the initial dip; it doesn't
			// disambiguate anything on its own, so keep looking past it.
			continue
		}
		return v >= peak-pctJitterPP
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
		       session_pct_valid, week_pct_valid, account_id
		FROM usage_observations
		ORDER BY account_id ASC, ts_unix_ms ASC, id ASC
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
		accts      []int64
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
			acct               int64
		)
		if err := rows.Scan(&id, &tsMS, &sPct, &wPct, &sResetTS, &wResetTS,
			&sReset, &wReset, &sValid, &wValid, &acct); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
		accts = append(accts, acct)
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

	// Each account is its own meter, so each is its own series. Rows arrive
	// grouped by account; classify each run separately, or one account's
	// 80% next to another's 10% reads as a reset every other reading.
	sessFlags := make([]pctFlags, 0, len(ids))
	weekFlags := make([]pctFlags, 0, len(ids))
	for lo := 0; lo < len(ids); {
		hi := lo
		for hi < len(ids) && accts[hi] == accts[lo] {
			hi++
		}
		sessFlags = append(sessFlags, classifyBucket(sess[lo:hi])...)
		weekFlags = append(weekFlags, classifyBucket(week[lo:hi])...)
		lo = hi
	}

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
