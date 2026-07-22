package aggregate

import (
	"context"
	"database/sql"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/attribute"
	"github.com/PeterSR/claude-code-bloodhound/internal/costweight"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

// calibrationBucket maps an attribution bucket onto the name
// calibration_points uses. The two vocabularies differ on purpose: the
// calibrator predates per-session work and calls the 5-hour window
// "session", which is the one word this feature could not reuse.
func calibrationBucket(b string) string {
	if b == attribute.BucketWeek {
		return "week"
	}
	return "session"
}

// refreshAttribution rebuilds limit_windows + session_attribution for both
// limit meters: whose conversation moved the 5h meter, and whose moved the
// weekly one.
//
// Both buckets read the same two inputs (the /usage series and every turn),
// so they are loaded once and reconstructed twice. Each bucket is written in
// its own transaction: a weekly rebuild that fails leaves the 5h view
// standing rather than blanking the whole page.
func refreshAttribution(ctx context.Context, s *store.Store) (int, error) {
	turns, err := loadAttributionTurns(ctx, s)
	if err != nil {
		return 0, err
	}
	sessObs, weekObs, err := loadAttributionObs(ctx, s)
	if err != nil {
		return 0, err
	}

	nowMS := time.Now().UnixMilli()
	written := 0
	for _, b := range []struct {
		bucket string
		obs    []attribute.Obs
	}{
		{attribute.Bucket5h, sessObs},
		{attribute.BucketWeek, weekObs},
	} {
		// The calibration median only backs the estimated fallback. Its
		// absence is not an error: a fresh install has no calibration yet,
		// and every row simply comes out with tokens but no percentage until
		// enough /usage observations accumulate.
		var tokensPerPct float64
		if cw, _, _, ok, err := s.LatestCalibrationMedian(ctx, calibrationBucket(b.bucket), 10); err != nil {
			return written, err
		} else if ok {
			tokensPerPct = cw
		}

		res := attribute.Build(b.obs, turns, attribute.ParamsFor(b.bucket, nowMS, tokensPerPct))

		windows := make([]store.LimitWindowRow, 0, len(res.Windows))
		for _, w := range res.Windows {
			windows = append(windows, store.LimitWindowRow{
				Bucket:         w.Bucket,
				StartUnixMS:    w.StartUnixMS,
				EndUnixMS:      w.EndUnixMS,
				ResetUnixMS:    w.ResetUnixMS,
				Inferred:       w.Inferred,
				Partial:        w.Partial,
				InProgress:     w.InProgress,
				MeasuredPct:    w.MeasuredPct,
				AttributedPct:  w.AttributedPct,
				PeakPct:        w.PeakPct,
				HitCap:         w.HitCap,
				TokensPerPctCW: w.TokensPerPctCW,
			})
		}
		rows := make([]store.AttributionRow, 0, len(res.Rows))
		for _, r := range res.Rows {
			rows = append(rows, store.AttributionRow{
				Bucket:            r.Bucket,
				WindowStartUnixMS: r.WindowStartUnixMS,
				SessionUUID:       r.SessionUUID,
				Project:           r.Project,
				MeasuredPct:       r.MeasuredPct,
				EstimatedPct:      r.EstimatedPct,
				CWTokens:          r.CWTokens,
				RawTokens:         r.RawTokens,
				TurnCount:         r.TurnCount,
				FirstTSUnixMS:     r.FirstTSUnixMS,
				LastTSUnixMS:      r.LastTSUnixMS,
			})
		}
		if err := s.ReplaceAttribution(ctx, b.bucket, windows, rows); err != nil {
			return written, err
		}
		written += len(rows)
	}
	return written, nil
}

// loadAttributionTurns pulls every turn's session, project and weighted size.
// Cost weighting happens in Go rather than SQL so it goes through the exact
// same costweight.CW the calibrator uses.
func loadAttributionTurns(ctx context.Context, s *store.Store) ([]attribute.Turn, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT session_uuid, project, ts_unix_ms, model,
		       input_tokens, output_tokens, cache_read,
		       cache_create_5m, cache_create_1h
		FROM turns
		ORDER BY ts_unix_ms ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []attribute.Turn
	for rows.Next() {
		var (
			uuid, project, model  string
			tsMS                  int64
			in, o, cr, cw5m, cw1h int64
		)
		if err := rows.Scan(&uuid, &project, &tsMS, &model, &in, &o, &cr, &cw5m, &cw1h); err != nil {
			return nil, err
		}
		out = append(out, attribute.Turn{
			TSUnixMS:    tsMS,
			SessionUUID: uuid,
			Project:     project,
			RawTokens:   in + o + cr + cw5m + cw1h,
			CWTokens:    costweight.CW(model, in, o, cr, cw5m, cw1h),
		})
	}
	return out, rows.Err()
}

// loadAttributionObs reads the /usage series once and splits it into the two
// per-bucket views the reconstruction wants.
func loadAttributionObs(ctx context.Context, s *store.Store) (sess, week []attribute.Obs, err error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT ts_unix_ms,
		       session_pct, week_pct,
		       session_pct_valid, week_pct_valid,
		       session_saturated, week_saturated,
		       session_reset_ts, week_reset_ts
		FROM usage_observations
		ORDER BY ts_unix_ms ASC, id ASC
	`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			tsMS                       int64
			sPct, wPct                 sql.NullInt64
			sValid, wValid, sSat, wSat int
			sReset, wReset             sql.NullString
		)
		if err := rows.Scan(&tsMS, &sPct, &wPct, &sValid, &wValid, &sSat, &wSat, &sReset, &wReset); err != nil {
			return nil, nil, err
		}
		sess = append(sess, attribute.Obs{
			TSUnixMS:  tsMS,
			Pct:       int(sPct.Int64),
			HasPct:    sPct.Valid,
			Valid:     sValid == 1,
			Saturated: sSat == 1,
			ResetTS:   sReset.String,
		})
		week = append(week, attribute.Obs{
			TSUnixMS:  tsMS,
			Pct:       int(wPct.Int64),
			HasPct:    wPct.Valid,
			Valid:     wValid == 1,
			Saturated: wSat == 1,
			ResetTS:   wReset.String,
		})
	}
	return sess, week, rows.Err()
}
