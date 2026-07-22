package aggregate

import (
	"context"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/attribute"
	"github.com/PeterSR/claude-code-bloodhound/internal/costweight"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

// refreshBuckets walks `turns` in time order and groups them into 5-hour
// windows. A window starts at the first turn after the previous window
// ended (5h after that window's start), or at the very first turn of the
// dataset.
//
// The most recent bucket gets its right edge anchored to the most recent
// `usage_observations.session_reset_ts` if one is available; older buckets
// are marked reset_inferred=1.
//
// This is the simplest reasonable model and matches the POC. A future
// refinement could anchor every historical bucket to nearby observation
// resets, but the simple form already drives the UI well.
func refreshBuckets(ctx context.Context, s *store.Store) (int, error) {
	// Anthropic's 5-hour limit window, in milliseconds. Derived from
	// attribute.SessPeriod rather than restated so this bucketing and
	// attribute's own window reconstruction can never disagree about how
	// long a window is.
	const fiveHMS int64 = int64(attribute.SessPeriod / time.Millisecond)

	rows, err := s.DB.QueryContext(ctx, `
		SELECT ts_unix_ms, model,
		       input_tokens, output_tokens, cache_read,
		       cache_create_5m, cache_create_1h
		FROM turns
		ORDER BY ts_unix_ms
	`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type acc struct {
		startMS, endMS int64
		raw, output    int64
		costWeighted   float64
		count          int
	}
	var buckets []acc
	var cur *acc
	for rows.Next() {
		var (
			tsMS                    int64
			model                   string
			in, out, cr, cw5m, cw1h int64
		)
		if err := rows.Scan(&tsMS, &model, &in, &out, &cr, &cw5m, &cw1h); err != nil {
			return 0, err
		}
		if cur == nil || tsMS >= cur.endMS {
			buckets = append(buckets, acc{startMS: tsMS, endMS: tsMS + fiveHMS})
			cur = &buckets[len(buckets)-1]
		}
		w := in + out + cr + cw5m + cw1h
		costW := costweight.CW(model, in, out, cr, cw5m, cw1h)
		cur.raw += w
		cur.output += out
		cur.costWeighted += costW
		cur.count++
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	// Anchor the most-recent bucket's right edge to the most recent parsed
	// session_reset_ts when available. The POC observed reset hints
	// like "2am" → "2026-04-29T00:00:00Z" landing in this column.
	var resetMS int64
	var resetMSValid bool
	if len(buckets) > 0 {
		var ts string
		err := s.DB.QueryRowContext(ctx, `
			SELECT session_reset_ts FROM usage_observations
			WHERE session_reset_ts IS NOT NULL
			ORDER BY ts_unix_ms DESC LIMIT 1
		`).Scan(&ts)
		if err == nil {
			if t, ok := parseISO(ts); ok {
				resetMS = t
				resetMSValid = true
			}
		}
	}

	out := make([]store.BucketRow, 0, len(buckets))
	for i, b := range buckets {
		row := store.BucketRow{
			StartUnixMS:       b.startMS,
			EndUnixMS:         b.endMS,
			ResetInferred:     true,
			RawTokenTotal:     b.raw,
			CostWeightedTotal: b.costWeighted,
			OutputTokenTotal:  b.output,
			TurnCount:         b.count,
		}
		if i == len(buckets)-1 && resetMSValid && resetMS > b.startMS {
			row.EndUnixMS = resetMS
			row.ResetInferred = false
		}
		out = append(out, row)
	}

	if err := s.ReplaceBuckets(ctx, out); err != nil {
		return 0, err
	}
	return len(out), nil
}

// parseISO is a small ISO-8601 helper that returns unix ms.
func parseISO(s string) (int64, bool) {
	t, ok := tryParse(s)
	if !ok {
		return 0, false
	}
	return t.UnixMilli(), true
}
