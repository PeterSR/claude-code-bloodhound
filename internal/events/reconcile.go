package events

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Stats summarises one reconcile pass.
type Stats struct {
	SensorsRun   int      `json:"sensors_run"`
	ReadingsSeen int      `json:"readings_seen"`
	Emitted      int      `json:"emitted"`
	HooksFired   int      `json:"hooks_fired"`
	HooksExpired int      `json:"hooks_expired"`
	Errors       []string `json:"errors,omitempty"`
}

// levelKey identifies one level track.
type levelKey struct {
	kind, bucket, session, cwd string
	account                    int64
}

// Reconcile runs every registered sensor once, diffs each reading against
// event_levels, appends an event per transition, then fires any one-shot hooks
// the new events matched.
//
// A sensor returning an error is skipped and recorded, never fatal: one broken
// sensor must not stop the others from recording. That matters more here than
// usual, because the sensors most likely to break are the ones reporting that
// collection itself is broken.
//
// Hook firing happens after the commit, never inside it, so a handler that
// blocks cannot hold a write transaction open.
func Reconcile(ctx context.Context, db *sql.DB, w World) (Stats, error) {
	var st Stats

	readings := make([]Reading, 0, 16)
	for _, s := range Registered() {
		st.SensorsRun++
		rs, err := s.Read(ctx, w)
		if err != nil {
			st.Errors = append(st.Errors, fmt.Sprintf("sensor %s: %v", s.Name, err))
			continue
		}
		readings = append(readings, rs...)
	}
	st.ReadingsSeen = len(readings)

	if err := applyReadings(ctx, db, w.Now, readings, &st); err != nil {
		return st, err
	}

	// Hooks are matched from each pending hook's own cursor rather than from
	// whatever this pass appended. Edges land outside reconcile entirely
	// (store.RecordUsage appends window.reset inside its own transaction), and
	// scoping to this pass would miss exactly the event that motivated
	// one-shots in the first place. It is also what makes a fire that is late
	// by a suspend still happen.
	fired, expired, errs := runHooks(ctx, db, w.Now)
	st.HooksFired = fired
	st.HooksExpired = expired
	st.Errors = append(st.Errors, errs...)
	return st, nil
}

// busyRetries bounds how many times a reconcile pass re-runs after losing a
// write race.
//
// The transaction starts as a deferred read (the levels) and only then writes,
// so SQLite can invalidate its snapshot when another process commits in
// between, and busy_timeout does not help with that: the answer is SQLITE_BUSY
// immediately, not after a wait. Two processes reconciling at once is an
// ordinary configuration here (the daemon's 60s tick alongside `bloodhound
// poll` on cron), so losing the race has to be survivable rather than an error
// sprayed into cron mail.
const busyRetries = 4

func isBusy(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "SQLITE_BUSY") || strings.Contains(s, "database is locked")
}

// applyReadings does the diff and the appends in one transaction, retrying the
// whole pass if it loses a write race.
func applyReadings(ctx context.Context, db *sql.DB, now time.Time, readings []Reading, st *Stats) error {
	if len(readings) == 0 {
		return nil
	}
	var err error
	for attempt := 0; attempt < busyRetries; attempt++ {
		emittedBefore := st.Emitted
		err = applyReadingsOnce(ctx, db, now, readings, st)
		if !isBusy(err) {
			return err
		}
		// The failed attempt rolled back, so anything it counted did not
		// happen. Reset before retrying or the stats double-count.
		st.Emitted = emittedBefore
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(50*(attempt+1)) * time.Millisecond):
		}
	}
	return err
}

func applyReadingsOnce(ctx context.Context, db *sql.DB, now time.Time, readings []Reading, st *Stats) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	current := map[levelKey]string{}
	rows, err := tx.QueryContext(ctx, `SELECT kind, bucket, session_uuid, cwd, account_id, state FROM event_levels`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var k levelKey
		var state string
		if err := rows.Scan(&k.kind, &k.bucket, &k.session, &k.cwd, &k.account, &state); err != nil {
			rows.Close()
			return err
		}
		current[k] = state
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	tsMS := now.UnixMilli()

	for _, r := range readings {
		state := r.State
		if state == "" {
			state = StateUnknown
		}
		k := levelKey{r.Kind, r.Scope.Bucket, r.Scope.Session, r.Scope.Cwd, r.Scope.Account}

		prev, had := current[k]
		if had && prev == state {
			continue // no transition, nothing to say
		}
		// A level appearing for the first time is not a transition worth
		// announcing when it appears as "unknown": that is just the reconciler
		// meeting a fact it cannot evaluate yet, and emitting on it would fill
		// a fresh install's log with noise before the first poll lands.
		if !had && state == StateUnknown {
			current[k] = state
			if err := upsertLevel(ctx, tx, k, state, tsMS); err != nil {
				return err
			}
			continue
		}

		// A level seen for the first time reports "unknown" as where it came
		// from, which is true and keeps an empty prev_state meaning exactly
		// one thing: this is an edge, which has no previous state at all.
		if !had {
			prev = StateUnknown
		}
		if _, err := appendWithPrev(ctx, tx, tsMS, Kind(r.Kind, state), prev, r.Scope, r.Detail); err != nil {
			return err
		}
		st.Emitted++
		current[k] = state
		if err := upsertLevel(ctx, tx, k, state, tsMS); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func upsertLevel(ctx context.Context, ex execer, k levelKey, state string, tsMS int64) error {
	_, err := ex.ExecContext(ctx, `
		INSERT INTO event_levels (kind, bucket, session_uuid, cwd, account_id, state, since_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(kind, bucket, session_uuid, cwd, account_id)
		DO UPDATE SET state = excluded.state, since_ms = excluded.since_ms`,
		k.kind, k.bucket, k.session, k.cwd, k.account, state, tsMS)
	return err
}
