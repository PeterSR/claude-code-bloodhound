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
		// parentSessionUUID is constant across every turn in a session
		// (denormalized onto each turn row the same way project is), so we
		// only need to capture it once, at accumulator creation.
		parentSessionUUID string
		// cwd is NOT constant the way project and parentSessionUUID are:
		// project comes from the transcript's file path (one value per
		// file, by construction), but cwd is read off each JSONL record and
		// tracks wherever the agent's tools actually were at that moment,
		// which can change turn to turn (cd into a subdirectory, a monorepo
		// package, even a different checkout). sessions.cwd needs a single
		// value, so we pick the most common one across the session's turns:
		// it best reflects where the bulk of the session's actual work (and
		// so its actual spend) happened. Rejected alternatives: the first
		// turn's cwd (the launch directory) can be stale for the rest of a
		// session that immediately moved elsewhere; the last turn's cwd (a
		// single `cd` back to the launch dir at the very end would mislabel
		// a session that did all its real work in between). Most-common is
		// also naturally robust to a brief excursion (one `cd /tmp && ...`)
		// that neither of the other two rules resists on its own.
		//
		// Empty cwd values (missing on old records, or turns whose JSONL
		// predates this field) are excluded from the count entirely rather
		// than being allowed to win by volume: a session with some tagged
		// and some untagged turns should report the real directory, not "".
		// cwdOrder preserves first-seen order so a tie resolves to whichever
		// directory the session visited first, deterministically, rather
		// than depending on Go's map iteration order.
		cwdCounts map[string]int
		cwdOrder  []string
		// accountID is the account of the session's latest turn. A session
		// that spans a /login switch is filed under where it ended up.
		accountID int64
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
		       parent_session_uuid, cwd, account_id
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
			accountID                   int64
		)
		if err := rows.Scan(&uuid, &project, &tsMS, &model, &in, &out, &cr, &cw5m, &cw1h, &class, &parentUUID, &cwd, &accountID); err != nil {
			return 0, err
		}
		a, ok := acc[uuid]
		if !ok {
			a = &sessAcc{
				project:           project,
				firstTSMS:         tsMS,
				modelsSeen:        map[string]bool{},
				parentSessionUUID: parentUUID,
				cwdCounts:         map[string]int{},
			}
			acc[uuid] = a
		}
		if cwd != "" {
			if _, seen := a.cwdCounts[cwd]; !seen {
				a.cwdOrder = append(a.cwdOrder, cwd)
			}
			a.cwdCounts[cwd]++
		}
		a.lastTSMS = tsMS
		a.accountID = accountID
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

		// Most common cwd wins; see the field doc on sessAcc for why. Ties
		// (equal counts) resolve to whichever was seen first, since we only
		// overwrite on a strictly greater count.
		var cwd string
		if len(a.cwdOrder) > 0 {
			cwd = a.cwdOrder[0]
			best := a.cwdCounts[cwd]
			for _, c := range a.cwdOrder[1:] {
				if n := a.cwdCounts[c]; n > best {
					cwd, best = c, n
				}
			}
		}

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
			Cwd:                 cwd,
			AccountID:           a.accountID,
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
