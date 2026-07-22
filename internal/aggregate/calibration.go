package aggregate

import (
	"context"
	"database/sql"
	"sort"

	"github.com/PeterSR/claude-code-bloodhound/internal/costweight"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

// refreshCalibration rebuilds calibration_points from usage_observations +
// turns.
//
// This used to walk *adjacent pairs* of observations and emit a point
// wherever the displayed integer ticked (b.pct > a.pct), charging that one
// point against only the tokens spent in that single poll interval. That was
// biased in two independent, compounding ways:
//
//  1. Span mismatch. /usage reports whole percents and the daemon polls every
//     five minutes, so most polls are flat: tokens were spent but the integer
//     didn't move. The pairwise walk skipped every flat pair (delta <= 0) and
//     dropped its tokens on the floor entirely, while whichever pair DID tick
//     got charged for one poll interval's tokens against a whole point of
//     movement. The denominator counted every point the meter moved; the
//     numerator counted only the tokens spent in the instant it happened to
//     be observed. The numerator systematically starved relative to the
//     denominator.
//  2. Jitter double-count. The meter isn't monotonic between polls (misread
//     glitches, or the window legitimately wobbling a point or two), and a
//     pairwise walk has no memory of where the run has already been. Every
//     re-rise after a dip counted as fresh spend, even when it was only
//     regaining ground an earlier reading already reflected.
//
// Both are fixed by the same idea already used twice elsewhere in this
// codebase (attribute.measureWindow / attribute.priceWindows, and capacity.go's
// weeklyCapacity): measure the numerator and denominator over the same span,
// against a running peak that only ever rises within a window, instead of
// over whichever pair happened to tick. See buildCalibrationPoints.
func refreshCalibration(ctx context.Context, s *store.Store) (int, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM calibration_points`); err != nil {
		return 0, err
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT id, ts_unix_ms,
		       session_pct, week_pct,
		       session_saturated, week_saturated,
		       session_reset_detected, week_reset_detected,
		       session_pct_valid, week_pct_valid
		FROM usage_observations
		ORDER BY ts_unix_ms ASC, id ASC
	`)
	if err != nil {
		return 0, err
	}
	var sessionObs, weekObs []calObs
	for rows.Next() {
		var (
			id, tsMS       int64
			sp, wp         sql.NullInt64
			ss, ws, sr, wr int
			sv, wv         int
		)
		if err := rows.Scan(&id, &tsMS, &sp, &wp, &ss, &ws, &sr, &wr, &sv, &wv); err != nil {
			rows.Close()
			return 0, err
		}
		sessionObs = append(sessionObs, calObs{
			ID: id, TSUnixMS: tsMS,
			Pct: int(sp.Int64), HasPct: sp.Valid,
			Valid: sv == 1, Saturated: ss == 1, Reset: sr == 1,
		})
		weekObs = append(weekObs, calObs{
			ID: id, TSUnixMS: tsMS,
			Pct: int(wp.Int64), HasPct: wp.Valid,
			Valid: wv == 1, Saturated: ws == 1, Reset: wr == 1,
		})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	if len(sessionObs) < 2 {
		return 0, tx.Commit()
	}

	// Pre-load all turns once; buildCalibrationPoints binary-searches them
	// per run rather than scanning, so this is a single pass regardless of
	// how many runs each bucket produces. Even at 100k turns this is cheap
	// (~16MB).
	trows, err := tx.QueryContext(ctx, `
		SELECT ts_unix_ms, model,
		       input_tokens, output_tokens, cache_read,
		       cache_create_5m, cache_create_1h
		FROM turns
		ORDER BY ts_unix_ms ASC
	`)
	if err != nil {
		return 0, err
	}
	var turns []calTurn
	for trows.Next() {
		var (
			tsMS                    int64
			model                   string
			in, out, cr, cw5m, cw1h int64
		)
		if err := trows.Scan(&tsMS, &model, &in, &out, &cr, &cw5m, &cw1h); err != nil {
			trows.Close()
			return 0, err
		}
		raw := in + out + cr + cw5m + cw1h
		cw := costweight.CW(model, in, out, cr, cw5m, cw1h)
		turns = append(turns, calTurn{TSUnixMS: tsMS, Raw: raw, Out: out, CW: cw})
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

	insert := func(bucket string, pts []calPoint) error {
		for _, p := range pts {
			if _, err := stmt.ExecContext(ctx,
				bucket, p.AObsID, p.BObsID,
				p.ATSUnixMS, p.BTSUnixMS,
				p.APct, p.BPct, p.DeltaPct,
				p.RawTokens, p.CostWeightedTokens, p.OutputTokens, p.TurnCount,
				p.GapS,
				p.TokensPerPctRaw, p.TokensPerPctCW,
			); err != nil {
				return err
			}
		}
		return nil
	}

	sessionPoints := buildCalibrationPoints(sessionObs, turns)
	weekPoints := buildCalibrationPoints(weekObs, turns)
	if err := insert("session", sessionPoints); err != nil {
		return 0, err
	}
	if err := insert("week", weekPoints); err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(sessionPoints) + len(weekPoints), nil
}

// calObs is one bucket's view of a /usage observation: whichever of
// session_* / week_* the caller picked off usage_observations, plus that
// bucket's own reset/saturation/validity flags. Mirrors attribute.Obs, which
// carries the same fields for the same reasons; the field names deliberately
// match.
type calObs struct {
	ID       int64
	TSUnixMS int64
	Pct      int
	HasPct   bool // the column was non-NULL
	// Valid is *_pct_valid: false marks a reading the misparse classifier
	// rejected, which must not anchor anything or move the peak.
	Valid bool
	// Saturated marks a reading pinned at the bucket's cap, where the meter
	// stops moving while tokens keep being spent. It breaks the run being
	// measured but, unlike Reset, does not forget the peak.
	Saturated bool
	// Reset is *_reset_detected: the meter rolled back to (near) zero, so
	// the pcts either side of it are not comparable. Starts a fresh window
	// and forgets the peak.
	Reset bool
}

// calTurn is one turn's contribution, reduced to what calibration needs.
type calTurn struct {
	TSUnixMS int64
	Raw      int64
	Out      int64
	CW       float64
}

// calPoint is one row calibration_points will receive.
type calPoint struct {
	AObsID, BObsID       int64
	ATSUnixMS, BTSUnixMS int64
	// APct/BPct are the run's entry and exit peak, not necessarily those
	// observations' own readings: a run that starts low and never beats an
	// earlier peak enters already "at" that peak (see buildCalibrationPoints).
	APct, BPct, DeltaPct int64
	RawTokens            int64
	CostWeightedTokens   float64
	OutputTokens         int64
	TurnCount            int
	GapS                 float64
	TokensPerPctRaw      float64
	TokensPerPctCW       float64
}

// buildCalibrationPoints walks one bucket's observations in time order,
// maintaining a running peak that only ever rises within a window, and
// returns one calPoint per *run*: a maximal span between a reset or
// saturation break and the next one (or an end of the series).
//
// A detected reset starts a fresh window and forgets the peak, since the
// pcts either side of it are not comparable — the meter rolled back to
// (near) zero. A saturated reading breaks the run being measured, because
// the meter is pinned at the cap and stops reflecting spend across it, but
// does NOT forget the peak: real usage did not go backwards just because the
// display clamped. An invalid reading may not anchor a run or move its peak,
// but it also may not break an otherwise measurable run — it is simply
// invisible to the walk, exactly as attribute.measureWindow treats one.
//
// A run's movement is max(0, peak - entry), where entry is the higher of the
// run's own first usable reading and whatever peak carried in from before,
// so regaining ground a previous run already reached contributes nothing.
// a_obs_id/b_obs_id are the run's first and last observations (literal
// bounds, not necessarily the usable ones); the tokens summed into the
// numerator are exactly those in (a.ts, b.ts], so the numerator always
// covers the same span the delta was measured over.
//
// obs and turns need not be pre-sorted.
func buildCalibrationPoints(obs []calObs, turns []calTurn) []calPoint {
	obs = append([]calObs(nil), obs...)
	sort.Slice(obs, func(i, j int) bool { return obs[i].TSUnixMS < obs[j].TSUnixMS })
	turns = append([]calTurn(nil), turns...)
	sort.Slice(turns, func(i, j int) bool { return turns[i].TSUnixMS < turns[j].TSUnixMS })

	// rangeIn returns the [from, to) index range of turns in the half-open
	// interval (lo, hi].
	rangeIn := func(lo, hi int64) (int, int) {
		from := sort.Search(len(turns), func(i int) bool { return turns[i].TSUnixMS > lo })
		to := sort.Search(len(turns), func(i int) bool { return turns[i].TSUnixMS > hi })
		if to < from {
			to = from
		}
		return from, to
	}

	var (
		points   []calPoint
		peak     int
		havePeak bool
	)

	emitRun := func(run []calObs) {
		if len(run) == 0 {
			return
		}
		loPct, runPeak, ok := runAnchors(run)
		if !ok {
			// Nothing in this run can anchor a measurement (every reading in
			// it was invalid or NULL). Its tokens are simply not calibrated;
			// the carried peak is left alone so the next run still measures
			// against the last real high point, bridging over the gap
			// instead of resetting through it.
			return
		}
		entry := loPct
		if havePeak && peak > entry {
			// The run started at or below ground already seen; regaining it
			// is not new spend.
			entry = peak
		}
		if runPeak < entry {
			runPeak = entry
		}
		delta := runPeak - entry
		peak, havePeak = runPeak, true
		if delta <= 0 {
			return
		}

		a, b := run[0], run[len(run)-1]
		from, to := rangeIn(a.TSUnixMS, b.TSUnixMS)
		var raw, outTok int64
		var cw float64
		var turnCount int
		for i := from; i < to; i++ {
			raw += turns[i].Raw
			outTok += turns[i].Out
			cw += turns[i].CW
			turnCount++
		}
		if raw == 0 {
			// The meter moved but nothing we ingested explains it — another
			// machine on the account, or a JSONL we never read. No
			// calibration signal here.
			return
		}
		points = append(points, calPoint{
			AObsID: a.ID, BObsID: b.ID,
			ATSUnixMS: a.TSUnixMS, BTSUnixMS: b.TSUnixMS,
			APct: int64(entry), BPct: int64(runPeak), DeltaPct: int64(delta),
			RawTokens: raw, CostWeightedTokens: cw, OutputTokens: outTok, TurnCount: turnCount,
			GapS:            float64(b.TSUnixMS-a.TSUnixMS) / 1000.0,
			TokensPerPctRaw: float64(raw) / float64(delta),
			TokensPerPctCW:  cw / float64(delta),
		})
	}

	runStart := 0
	for i, o := range obs {
		switch {
		case o.Reset:
			// The reset-flagged reading itself starts the new window (it is
			// never allowed to anchor the OLD run as an ending point, only
			// to open the new one): close the run before it, then let it be
			// the new run's first element with the peak forgotten.
			emitRun(obs[runStart:i])
			runStart = i
			havePeak = false
		case o.Saturated:
			// Excluded from every run: the meter's pinned there, so nothing
			// spans it safely.
			emitRun(obs[runStart:i])
			runStart = i + 1
		}
	}
	emitRun(obs[runStart:])
	return points
}

// runAnchors finds the entry candidate (the first usable reading's own
// clamped pct) and the run's peak (the highest usable reading anywhere in
// it). ok is false when the run has no usable reading at all.
func runAnchors(run []calObs) (loPct, runPeak int, ok bool) {
	for _, o := range run {
		if !o.HasPct || !o.Valid {
			continue
		}
		v := o.Pct
		if v > 100 {
			v = 100
		}
		if !ok {
			loPct, runPeak, ok = v, v, true
			continue
		}
		if v > runPeak {
			runPeak = v
		}
	}
	return
}
