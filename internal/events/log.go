package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"path"
	"time"
)

// execer is satisfied by both *sql.DB and *sql.Tx, which is what lets an edge
// be appended from inside a caller's existing transaction.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// queryer is the read half, likewise satisfied by both.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// AppendTx records one event. kind is the full dotted name.
//
// Takes an execer rather than a store handle on purpose: an edge should only
// ever be recorded as part of the write that discovered it, so there is no
// window where the fact is in the database but the event is not.
func AppendTx(ctx context.Context, ex execer, tsMS int64, kind string, sc Scope, detail map[string]any) (int64, error) {
	return appendWithPrev(ctx, ex, tsMS, kind, "", sc, detail)
}

func appendWithPrev(ctx context.Context, ex execer, tsMS int64, kind, prev string, sc Scope, detail map[string]any) (int64, error) {
	var detailJSON string
	if len(detail) > 0 {
		b, err := json.Marshal(detail)
		if err != nil {
			return 0, fmt.Errorf("marshal event detail: %w", err)
		}
		detailJSON = string(b)
	}
	r, err := ex.ExecContext(ctx, `
		INSERT INTO events (ts_unix_ms, kind, bucket, session_uuid, project, cwd, account_id, prev_state, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		tsMS, kind, sc.Bucket, sc.Session, sc.Project, sc.Cwd, sc.Account, prev, detailJSON,
	)
	if err != nil {
		return 0, err
	}
	id, _ := r.LastInsertId()
	return id, nil
}

// Filter selects a slice of the log. The zero value means "everything",
// bounded by Limit.
type Filter struct {
	SinceID int64    // strictly greater than
	SinceMS int64    // strictly greater than, ignored when zero
	UntilMS int64    // less than or equal to, ignored when zero
	Kinds   []string // globs; empty means all
	Bucket  string
	Session string
	Cwd     string
	Account int64 // 0 means any
	Limit   int   // <= 0 means DefaultLimit
}

// DefaultLimit bounds an unqualified read so a consumer that forgets to page
// cannot pull the whole table into memory.
const DefaultLimit = 500

// MatchKind reports whether kind satisfies any of the globs. An empty glob
// list matches everything.
func MatchKind(globs []string, kind string) bool {
	if len(globs) == 0 {
		return true
	}
	for _, g := range globs {
		if ok, err := path.Match(g, kind); err == nil && ok {
			return true
		}
	}
	return false
}

// Query returns matching events in ascending id order, oldest first, so a
// consumer replaying a gap sees transitions in the order they happened.
//
// Every predicate including the kind globs is pushed into SQL so LIMIT can do
// its job. Filtering globs in Go after an unbounded fetch was the earlier
// shape, and it meant a hook whose glob matched nothing scanned the whole
// table on every 60s tick.
//
// SQLite's GLOB operator has the same wildcards as path.Match, and event kinds
// contain no slashes, so the two agree on everything we store.
func Query(ctx context.Context, q queryer, f Filter) ([]Event, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}

	sqlStr := `SELECT id, ts_unix_ms, kind, bucket, session_uuid, project, cwd, account_id, prev_state, detail
	             FROM events WHERE id > ?`
	args := []any{f.SinceID}
	if f.SinceMS > 0 {
		sqlStr += ` AND ts_unix_ms > ?`
		args = append(args, f.SinceMS)
	}
	if f.UntilMS > 0 {
		sqlStr += ` AND ts_unix_ms <= ?`
		args = append(args, f.UntilMS)
	}
	if f.Bucket != "" {
		sqlStr += ` AND bucket = ?`
		args = append(args, f.Bucket)
	}
	if f.Session != "" {
		sqlStr += ` AND session_uuid = ?`
		args = append(args, f.Session)
	}
	if f.Cwd != "" {
		sqlStr += ` AND cwd = ?`
		args = append(args, f.Cwd)
	}
	if f.Account != 0 {
		sqlStr += ` AND account_id = ?`
		args = append(args, f.Account)
	}
	if len(f.Kinds) > 0 {
		clause := ""
		for i, g := range f.Kinds {
			if i > 0 {
				clause += " OR "
			}
			clause += "kind GLOB ?"
			args = append(args, g)
		}
		sqlStr += ` AND (` + clause + `)`
	}
	sqlStr += ` ORDER BY id ASC LIMIT ?`
	args = append(args, limit)

	rows, err := q.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Event{}
	for rows.Next() {
		var e Event
		var detailJSON string
		if err := rows.Scan(&e.ID, &e.TSUnixMS, &e.Kind, &e.Scope.Bucket,
			&e.Scope.Session, &e.Scope.Project, &e.Scope.Cwd, &e.Scope.Account, &e.PrevState, &detailJSON); err != nil {
			return nil, err
		}
		e.TSISO = time.UnixMilli(e.TSUnixMS).UTC().Format(time.RFC3339)
		if detailJSON != "" {
			_ = json.Unmarshal([]byte(detailJSON), &e.Detail)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MaxID returns the current cursor head, or 0 on an empty log. This is what
// "next" is measured from when a one-shot hook is registered.
func MaxID(ctx context.Context, q queryer) (int64, error) {
	var id sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT MAX(id) FROM events`).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id.Int64, nil
}

// Levels returns every current level, ordered for stable output.
func Levels(ctx context.Context, q queryer) ([]Level, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT kind, bucket, session_uuid, cwd, account_id, state, since_ms
		  FROM event_levels
		 ORDER BY kind, bucket, session_uuid, cwd, account_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Level{}
	for rows.Next() {
		var l Level
		if err := rows.Scan(&l.Kind, &l.Scope.Bucket, &l.Scope.Session, &l.Scope.Cwd, &l.Scope.Account, &l.State, &l.SinceMS); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// PruneLevels drops session-scoped level rows that have not moved since
// cutoff.
//
// Session levels are the one part of the table that grows without bound: a
// sensor that stops reporting a session (it aged out of the recent window)
// deliberately leaves its level alone, since absence is not evidence. That is
// correct per-tick and a leak over months. Meter levels carry no session uuid
// and are left alone, because a bucket sitting in the same state for a fortnight
// is ordinary rather than stale.
func PruneLevels(ctx context.Context, db *sql.DB, cutoffMS int64) (int64, error) {
	r, err := db.ExecContext(ctx,
		`DELETE FROM event_levels WHERE session_uuid != '' AND since_ms < ?`, cutoffMS)
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	return n, nil
}

// Prune deletes events older than cutoff, never dropping below keepFromID.
//
// keepFromID is the floor a caller passes so retention cannot silently swallow
// an event a pending consumer has not seen. Pass 0 when there is nothing to
// protect, which applies the age cutoff alone.
func Prune(ctx context.Context, db *sql.DB, cutoffMS, keepFromID int64) (int64, error) {
	if keepFromID <= 0 {
		keepFromID = math.MaxInt64
	}
	r, err := db.ExecContext(ctx,
		`DELETE FROM events WHERE ts_unix_ms < ? AND id < ?`, cutoffMS, keepFromID)
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	return n, nil
}
