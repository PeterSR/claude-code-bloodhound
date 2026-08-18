package store

import (
	"context"
	"database/sql"
	"time"
)

// Outcomes a promise can end in. The empty string is the fourth state and the
// only one that is not an ending: still pending.
const (
	// WakeupDelivered: the session was written to when the window reopened.
	WakeupDelivered = "delivered"
	// WakeupExpired: the deadline came and went far enough that writing would
	// be an interruption rather than a resumption, or nobody was listening for
	// long enough that there is plainly nothing there any more.
	WakeupExpired = "expired"
	// WakeupSuperseded: the window reopened but another window this session
	// was warned about is still shut, so waking it would hand it back the same
	// wall it stopped at. The later promise carries it instead.
	WakeupSuperseded = "superseded"
)

// SessionWakeup is one promise: this session, this window, this deadline.
type SessionWakeup struct {
	ID          int64
	SessionUUID string
	PID         int
	Cwd         string
	Bucket      string
	// Reason is the pressure line that caused the stop, so the message on the
	// far side can say what it is resuming from.
	Reason     string
	ArmedMS    int64
	DueMS      int64
	ExpireMS   int64
	Attempts   int
	Outcome    string
	ResolvedMS int64
	Note       string
}

// Armed returns the moment the promise was made.
func (w SessionWakeup) Armed() time.Time { return time.UnixMilli(w.ArmedMS) }

// Due returns the moment the window reopens.
func (w SessionWakeup) Due() time.Time { return time.UnixMilli(w.DueMS) }

// ArmWakeup records that a session was told to stop and will be written to
// when its window reopens.
//
// A pending promise for the same session and window is refreshed rather than
// duplicated. Pressure that crosses twice inside one window is one stop with
// one far side, and the newer reading has the better deadline: a window whose
// reset moved (a poll landing between two warnings can revise it) should wake
// the session at the moment it actually reopens, not the moment the first
// warning guessed.
func (s *Store) ArmWakeup(ctx context.Context, w SessionWakeup) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `
		INSERT INTO session_wakeups
		    (session_uuid, pid, cwd, bucket, reason, armed_ms, due_ms, expire_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (session_uuid, bucket) WHERE outcome = ''
		DO UPDATE SET
		    pid       = excluded.pid,
		    cwd       = excluded.cwd,
		    reason    = excluded.reason,
		    due_ms    = excluded.due_ms,
		    expire_ms = excluded.expire_ms`,
		w.SessionUUID, w.PID, w.Cwd, w.Bucket, w.Reason, w.ArmedMS, w.DueMS, w.ExpireMS)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// PendingWakeups returns every promise not yet resolved, oldest deadline
// first.
//
// The whole pending set rather than only what is due, because deciding
// whether a due promise should fire needs to see the others: a session warned
// about both windows must not be woken by the first one reopening while the
// second is still shut. The set is bounded by how many sessions are running,
// so reading all of it costs nothing.
func (s *Store) PendingWakeups(ctx context.Context) ([]SessionWakeup, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, session_uuid, pid, cwd, bucket, reason,
		       armed_ms, due_ms, expire_ms, attempts, note
		  FROM session_wakeups
		 WHERE outcome = ''
		 ORDER BY due_ms ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SessionWakeup
	for rows.Next() {
		var w SessionWakeup
		if err := rows.Scan(&w.ID, &w.SessionUUID, &w.PID, &w.Cwd, &w.Bucket, &w.Reason,
			&w.ArmedMS, &w.DueMS, &w.ExpireMS, &w.Attempts, &w.Note); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// RecentWakeups returns promises in reverse order of when they were made,
// pending ones included. What `bloodhound wakeups` prints.
func (s *Store) RecentWakeups(ctx context.Context, limit int) ([]SessionWakeup, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, session_uuid, pid, cwd, bucket, reason,
		       armed_ms, due_ms, expire_ms, attempts, outcome,
		       COALESCE(resolved_ms, 0), note
		  FROM session_wakeups
		 ORDER BY armed_ms DESC, id DESC
		 LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SessionWakeup
	for rows.Next() {
		var w SessionWakeup
		if err := rows.Scan(&w.ID, &w.SessionUUID, &w.PID, &w.Cwd, &w.Bucket, &w.Reason,
			&w.ArmedMS, &w.DueMS, &w.ExpireMS, &w.Attempts, &w.Outcome,
			&w.ResolvedMS, &w.Note); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ResolveWakeup ends a promise one way or another.
func (s *Store) ResolveWakeup(ctx context.Context, id int64, outcome string, nowMS int64, note string) error {
	_, err := s.DB.ExecContext(ctx, `
		UPDATE session_wakeups
		   SET outcome = ?, resolved_ms = ?, note = ?
		 WHERE id = ? AND outcome = ''`,
		outcome, nowMS, note, id)
	return err
}

// TouchWakeup records a failed attempt without ending the promise.
//
// Kept pending on purpose. The common reason a due promise cannot be
// delivered is that the machine is between states (the session's socket is
// there but not answering, the registry is mid-write), and the next tick is a
// minute away. Giving up on the first refusal would turn a transient miss into
// a broken promise; expire_ms is what eventually stops the retrying.
func (s *Store) TouchWakeup(ctx context.Context, id int64, note string) error {
	_, err := s.DB.ExecContext(ctx, `
		UPDATE session_wakeups
		   SET attempts = attempts + 1, note = ?
		 WHERE id = ? AND outcome = ''`, note, id)
	return err
}

// CancelWakeups resolves every pending promise, or only those for one
// session. What `bloodhound wakeups clear` calls, and the escape hatch for
// anyone who does not want to be written to after all.
func (s *Store) CancelWakeups(ctx context.Context, sessionUUID string, nowMS int64) (int64, error) {
	var (
		res sql.Result
		err error
	)
	if sessionUUID == "" {
		res, err = s.DB.ExecContext(ctx, `
			UPDATE session_wakeups
			   SET outcome = ?, resolved_ms = ?, note = 'cancelled by hand'
			 WHERE outcome = ''`, WakeupExpired, nowMS)
	} else {
		res, err = s.DB.ExecContext(ctx, `
			UPDATE session_wakeups
			   SET outcome = ?, resolved_ms = ?, note = 'cancelled by hand'
			 WHERE outcome = '' AND session_uuid = ?`, WakeupExpired, nowMS, sessionUUID)
	}
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// PruneWakeups drops resolved promises older than the cutoff. Pending ones
// are never touched, however old: a promise still waiting is the one thing in
// this table that is not history.
func (s *Store) PruneWakeups(ctx context.Context, cutoffMS int64) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `
		DELETE FROM session_wakeups
		 WHERE outcome <> '' AND COALESCE(resolved_ms, armed_ms) < ?`, cutoffMS)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
