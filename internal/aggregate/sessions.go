package aggregate

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/attribute"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

// refreshSessions rebuilds the `sessions` table from `turns` + `compactions`.
// Strategy: scan turns once, group by session_uuid in memory, then a single
// transaction wipes + reinserts. This is cheap (~50k turns ≪ MB of state).
func refreshSessions(ctx context.Context, s *store.Store) (int, error) {
	type sessAcc struct {
		project      string
		firstTSMS    int64
		lastTSMS     int64
		turnCount    int
		rawTokens    int64
		outputTokens int64
		idleMiss     int
		rotation     int
		restructure  int
		modelsSeen   map[string]bool
		// parentSessionUUID and cwd are constant across every turn in a
		// session (denormalized onto each turn row the same way project is),
		// so we only need to capture them once, at accumulator creation.
		parentSessionUUID string
		cwd               string
		// 5h-rolling
		weights []int64
		times   []int64
		// cache TTL classification
		cw5mTotal int64
		cw1hTotal int64
		// compactions filled in second pass
		compactionCount     int
		coldCompactionCount int
	}
	acc := map[string]*sessAcc{}

	rows, err := s.DB.QueryContext(ctx, `
		SELECT session_uuid, project, ts_unix_ms, model,
		       input_tokens, output_tokens, cache_read,
		       cache_create_5m, cache_create_1h, classification,
		       parent_session_uuid, cwd
		FROM turns
		ORDER BY session_uuid, ts_unix_ms
	`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			uuid, project, model, class string
			tsMS                        int64
			in, out, cr, cw5m, cw1h     int64
			parentUUID, cwd             string
		)
		if err := rows.Scan(&uuid, &project, &tsMS, &model, &in, &out, &cr, &cw5m, &cw1h, &class, &parentUUID, &cwd); err != nil {
			return 0, err
		}
		a, ok := acc[uuid]
		if !ok {
			a = &sessAcc{
				project:           project,
				firstTSMS:         tsMS,
				modelsSeen:        map[string]bool{},
				parentSessionUUID: parentUUID,
				cwd:               cwd,
			}
			acc[uuid] = a
		}
		a.lastTSMS = tsMS
		a.turnCount++
		w := in + out + cr + cw5m + cw1h
		a.rawTokens += w
		a.outputTokens += out
		a.cw5mTotal += cw5m
		a.cw1hTotal += cw1h
		if model != "" {
			a.modelsSeen[model] = true
		}
		switch class {
		case "idle_miss":
			a.idleMiss++
		case "rotation":
			a.rotation++
		case "restructure":
			a.restructure++
		}
		a.weights = append(a.weights, w)
		a.times = append(a.times, tsMS)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	// Compactions: count per session and how many were cold.
	crows, err := s.DB.QueryContext(ctx, `
		SELECT session_uuid, cache_state
		FROM compactions
		WHERE confirmed = 1
	`)
	if err != nil {
		return 0, err
	}
	defer crows.Close()
	for crows.Next() {
		var uuid, state string
		if err := crows.Scan(&uuid, &state); err != nil {
			return 0, err
		}
		a, ok := acc[uuid]
		if !ok {
			continue // compaction without any turns; ignore for sessions table
		}
		a.compactionCount++
		if state == "cold" {
			a.coldCompactionCount++
		}
	}

	// Compute peak 5h window per session and build the row set. Derived from
	// attribute.SessPeriod (Anthropic's 5-hour limit window), same as
	// buckets.go, so the two can never disagree about how long a window is.
	const fiveHMS int64 = int64(attribute.SessPeriod / time.Millisecond)
	type sessRow struct {
		store.SessionRow
	}
	rowsOut := make([]store.SessionRow, 0, len(acc))
	for uuid, a := range acc {
		var peak int64
		var lo int
		var sum int64
		for i, w := range a.weights {
			sum += w
			for lo < i && a.times[i]-a.times[lo] > fiveHMS {
				sum -= a.weights[lo]
				lo++
			}
			if sum > peak {
				peak = sum
			}
		}
		ttl := cacheTTL(a.cw5mTotal, a.cw1hTotal)
		models := make([]string, 0, len(a.modelsSeen))
		for m := range a.modelsSeen {
			models = append(models, m)
		}
		sort.Strings(models)

		rowsOut = append(rowsOut, store.SessionRow{
			SessionUUID:         uuid,
			Project:             a.project,
			FirstTSUnixMS:       a.firstTSMS,
			LastTSUnixMS:        a.lastTSMS,
			TurnCount:           a.turnCount,
			RawTokens:           a.rawTokens,
			OutputTokens:        a.outputTokens,
			Peak5hRawTokens:     peak,
			IdleMissCount:       a.idleMiss,
			RotationCount:       a.rotation,
			RestructureCount:    a.restructure,
			CompactionCount:     a.compactionCount,
			ColdCompactionCount: a.coldCompactionCount,
			CacheTTL:            ttl,
			Models:              strings.Join(models, ","),
			ParentSessionUUID:   a.parentSessionUUID,
			Cwd:                 a.cwd,
		})
	}

	if err := s.ReplaceSessions(ctx, rowsOut); err != nil {
		return 0, err
	}
	return len(rowsOut), nil
}

func cacheTTL(cw5m, cw1h int64) string {
	switch {
	case cw5m == 0 && cw1h == 0:
		return "none"
	case cw5m == 0:
		return "1h"
	case cw1h == 0:
		return "5m"
	default:
		return "mix"
	}
}
