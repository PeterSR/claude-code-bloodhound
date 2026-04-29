package store

import (
	"context"
)

// CalibrationPoint is one (a, b) pair worth of tokens-per-1% data.
type CalibrationPoint struct {
	ID                 int64
	Bucket             string // "session" | "week"
	ATSUnixMS          int64
	BTSUnixMS          int64
	APct               int
	BPct               int
	DeltaPct           int
	RawTokens          int64
	CostWeightedTokens float64
	OutputTokens       int64
	TurnCount          int
	GapS               float64
	TokensPerPctRaw    float64
	TokensPerPctCW     float64
}

// CalibrationPoints returns all calibration points for a bucket
// ('session' or 'week') in time order.
func (s *Store) CalibrationPoints(ctx context.Context, bucket string) ([]CalibrationPoint, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id,
		       a_ts_unix_ms, b_ts_unix_ms,
		       a_pct, b_pct, delta_pct,
		       raw_tokens, cost_weighted_tokens, output_tokens, turn_count,
		       gap_s,
		       tokens_per_pct_raw, tokens_per_pct_cw
		FROM calibration_points
		WHERE bucket = ?
		ORDER BY b_ts_unix_ms ASC
	`, bucket)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CalibrationPoint
	for rows.Next() {
		p := CalibrationPoint{Bucket: bucket}
		if err := rows.Scan(&p.ID,
			&p.ATSUnixMS, &p.BTSUnixMS,
			&p.APct, &p.BPct, &p.DeltaPct,
			&p.RawTokens, &p.CostWeightedTokens, &p.OutputTokens, &p.TurnCount,
			&p.GapS,
			&p.TokensPerPctRaw, &p.TokensPerPctCW,
		); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// LatestCalibrationMedian returns the median tokens-per-1% (cost-weighted
// and raw) over the most recent N points for a bucket. Returns
// (cwMedian, rawMedian, count, ok). ok is false when count == 0.
func (s *Store) LatestCalibrationMedian(ctx context.Context, bucket string, n int) (float64, float64, int, bool, error) {
	if n <= 0 {
		n = 10
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT tokens_per_pct_cw, tokens_per_pct_raw
		FROM calibration_points
		WHERE bucket = ?
		ORDER BY b_ts_unix_ms DESC
		LIMIT ?
	`, bucket, n)
	if err != nil {
		return 0, 0, 0, false, err
	}
	defer rows.Close()
	var cw, raw []float64
	for rows.Next() {
		var c, r float64
		if err := rows.Scan(&c, &r); err != nil {
			return 0, 0, 0, false, err
		}
		cw = append(cw, c)
		raw = append(raw, r)
	}
	if len(cw) == 0 {
		return 0, 0, 0, false, rows.Err()
	}
	return median(cw), median(raw), len(cw), true, rows.Err()
}

func median(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	cp := make([]float64, len(vals))
	copy(cp, vals)
	// insertion sort — len is small (≤ 50)
	for i := 1; i < len(cp); i++ {
		v := cp[i]
		j := i - 1
		for j >= 0 && cp[j] > v {
			cp[j+1] = cp[j]
			j--
		}
		cp[j+1] = v
	}
	mid := len(cp) / 2
	if len(cp)%2 == 1 {
		return cp[mid]
	}
	return (cp[mid-1] + cp[mid]) / 2
}
