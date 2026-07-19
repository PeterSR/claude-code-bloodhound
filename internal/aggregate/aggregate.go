// Package aggregate recomputes derived tables (sessions, buckets) from the
// raw rows ingested by internal/ingest. Aggregation is idempotent and runs
// on a slower cadence than ingest itself (e.g. every 15 minutes), which is
// why it lives in a separate package.
package aggregate

import (
	"context"
	"fmt"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

// Stats summarises one Run.
type Stats struct {
	SessionsRefreshed      int     `json:"sessions_refreshed"`
	BucketsRebuilt         int     `json:"buckets_rebuilt"`
	CalibrationPointsBuilt int     `json:"calibration_points_built"`
	ElapsedS               float64 `json:"elapsed_s"`
}

// Options configures Run.
type Options struct {
	// SkipSessions / SkipBuckets / SkipCalibration let callers run only
	// one phase. Mostly useful in tests; production always wants all
	// three.
	SkipSessions    bool
	SkipBuckets     bool
	SkipCalibration bool
}

// Run recomputes the materialized aggregate tables in one logical pass.
// Each phase runs in its own transaction so a failure in buckets doesn't
// roll back fresh session summaries.
func Run(ctx context.Context, s *store.Store, opts Options) (Stats, error) {
	t0 := time.Now()
	st := Stats{}

	// Reset/misparse flags are derived from the raw percentage series;
	// recompute them first so calibration (which skips resets and misparses)
	// and everything downstream see corrected values. This is also the
	// one-time backfill for observations recorded before the classifier
	// existed.
	if err := s.RecomputeUsageFlags(ctx); err != nil {
		return st, fmt.Errorf("recompute usage flags: %w", err)
	}

	if !opts.SkipSessions {
		n, err := refreshSessions(ctx, s)
		if err != nil {
			return st, fmt.Errorf("refresh sessions: %w", err)
		}
		st.SessionsRefreshed = n
	}

	if !opts.SkipBuckets {
		n, err := refreshBuckets(ctx, s)
		if err != nil {
			return st, fmt.Errorf("refresh buckets: %w", err)
		}
		st.BucketsRebuilt = n
	}

	if !opts.SkipCalibration {
		n, err := refreshCalibration(ctx, s)
		if err != nil {
			return st, fmt.Errorf("refresh calibration: %w", err)
		}
		st.CalibrationPointsBuilt = n
	}

	st.ElapsedS = time.Since(t0).Seconds()
	return st, nil
}
