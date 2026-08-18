package events

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Hook statuses.
const (
	HookPending   = "pending"
	HookFired     = "fired"
	HookExpired   = "expired"
	HookCancelled = "cancelled"
)

// HookTimeout bounds one handler. A one-shot that hangs must not stall the
// reconciler behind it.
const HookTimeout = 30 * time.Second

// Hook is one registered one-shot.
type Hook struct {
	ID           int64  `json:"id"`
	CreatedMS    int64  `json:"created_ms"`
	AfterEventID int64  `json:"after_event_id"`
	KindGlob     string `json:"kind"`
	Bucket       string `json:"bucket,omitempty"`
	Session      string `json:"session,omitempty"`
	Cwd          string `json:"cwd,omitempty"`
	Command      string `json:"command"`
	Note         string `json:"note,omitempty"`
	ExpiresMS    int64  `json:"expires_ms"`
	Status       string `json:"status"`
	FiredMS      *int64 `json:"fired_ms,omitempty"`
	FiredEventID *int64 `json:"fired_event_id,omitempty"`
	ExitCode     *int   `json:"exit_code,omitempty"`
}

// RegisterHook stores a one-shot. after_event_id is captured here rather than
// at fire time so that "next" means strictly after the moment of registration:
// without it, an event landing between registration and the next reconcile
// would either fire twice or not at all.
func RegisterHook(ctx context.Context, db *sql.DB, now time.Time, h Hook) (Hook, error) {
	head, err := MaxID(ctx, db)
	if err != nil {
		return Hook{}, err
	}
	h.AfterEventID = head
	h.CreatedMS = now.UnixMilli()
	h.Status = HookPending

	r, err := db.ExecContext(ctx, `
		INSERT INTO event_hooks
		  (created_ms, after_event_id, kind_glob, bucket, session_uuid, cwd, command, note, expires_ms, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		h.CreatedMS, h.AfterEventID, h.KindGlob, h.Bucket, h.Session, h.Cwd, h.Command, h.Note, h.ExpiresMS, h.Status,
	)
	if err != nil {
		return Hook{}, err
	}
	h.ID, _ = r.LastInsertId()
	return h, nil
}

// ListHooks returns hooks newest first. When pendingOnly, expired-but-unswept
// rows are excluded too, so the answer matches what would actually still fire.
func ListHooks(ctx context.Context, db *sql.DB, now time.Time, pendingOnly bool) ([]Hook, error) {
	q := `SELECT id, created_ms, after_event_id, kind_glob, bucket, session_uuid, cwd,
	             command, note, expires_ms, status, fired_ms, fired_event_id, exit_code
	        FROM event_hooks`
	var args []any
	if pendingOnly {
		q += ` WHERE status = ? AND expires_ms > ?`
		args = append(args, HookPending, now.UnixMilli())
	}
	q += ` ORDER BY id DESC`

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Hook{}
	for rows.Next() {
		var h Hook
		var firedMS, firedEvent sql.NullInt64
		var exit sql.NullInt64
		if err := rows.Scan(&h.ID, &h.CreatedMS, &h.AfterEventID, &h.KindGlob, &h.Bucket,
			&h.Session, &h.Cwd, &h.Command, &h.Note, &h.ExpiresMS, &h.Status,
			&firedMS, &firedEvent, &exit); err != nil {
			return nil, err
		}
		if firedMS.Valid {
			v := firedMS.Int64
			h.FiredMS = &v
		}
		if firedEvent.Valid {
			v := firedEvent.Int64
			h.FiredEventID = &v
		}
		if exit.Valid {
			v := int(exit.Int64)
			h.ExitCode = &v
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// CancelHook marks a pending hook cancelled. Returns false when there was no
// pending hook with that id.
func CancelHook(ctx context.Context, db *sql.DB, id int64) (bool, error) {
	r, err := db.ExecContext(ctx,
		`UPDATE event_hooks SET status = ? WHERE id = ? AND status = ?`,
		HookCancelled, id, HookPending)
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n > 0, nil
}

// OldestPendingCursor returns the lowest after_event_id across pending hooks,
// which is the floor retention must not prune below. Returns 0 when nothing is
// pending.
func OldestPendingCursor(ctx context.Context, db *sql.DB, now time.Time) (int64, error) {
	var v sql.NullInt64
	err := db.QueryRowContext(ctx,
		`SELECT MIN(after_event_id) FROM event_hooks WHERE status = ? AND expires_ms > ?`,
		HookPending, now.UnixMilli()).Scan(&v)
	if err != nil {
		return 0, err
	}
	return v.Int64, nil
}

// runHooks fires every pending hook whose filter matches an event past its own
// cursor, then sweeps whatever is left over its expiry.
//
// Matching happens BEFORE expiring, and matching is bounded by the hook's own
// expiry rather than by now. Both halves matter, and getting the order wrong
// breaks the case the feature exists for: a reset lands at hour 7 of an 8 hour
// one-shot, the laptop is closed from hour 6 to hour 9, and the hook must fire
// late on wake instead of being swept as expired without ever running.
//
// Claim before spawn: the row is marked fired inside a conditional UPDATE
// before the process starts, so a crash between the two loses one fire rather
// than repeating it on every subsequent reconcile. One shot means one shot.
func runHooks(ctx context.Context, db *sql.DB, now time.Time) (fired, expired int, errs []string) {
	nowMS := now.UnixMilli()

	// Everything still pending, including rows already past their expiry: one
	// of those may have a match that landed while it was still live.
	pending, err := listPendingRegardlessOfExpiry(ctx, db)
	if err != nil {
		return fired, expired, append(errs, fmt.Sprintf("list hooks: %v", err))
	}

	for _, h := range pending {
		matches, err := Query(ctx, db, Filter{
			SinceID: h.AfterEventID,
			UntilMS: h.ExpiresMS, // it had to land while the hook was live
			Kinds:   []string{h.KindGlob},
			Bucket:  h.Bucket,
			Session: h.Session,
			Cwd:     h.Cwd,
			Limit:   1,
		})
		if err != nil {
			errs = append(errs, fmt.Sprintf("hook %d match: %v", h.ID, err))
			continue
		}
		if len(matches) == 0 {
			continue
		}
		ev := matches[0]

		claimed, err := claimHook(ctx, db, h.ID, ev.ID, nowMS)
		if err != nil {
			errs = append(errs, fmt.Sprintf("hook %d claim: %v", h.ID, err))
			continue
		}
		if !claimed {
			continue // another reconcile got there first
		}

		code, output, err := execHook(ctx, h, ev, now)
		if err != nil {
			errs = append(errs, fmt.Sprintf("hook %d run: %v", h.ID, err))
		}
		// A handler that failed leaves only an exit code otherwise, which is
		// nothing to debug with. Surface a tail of what it said.
		if code != 0 && len(output) > 0 {
			errs = append(errs, fmt.Sprintf("hook %d exited %d: %s", h.ID, code, tailOf(output, 300)))
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE event_hooks SET exit_code = ? WHERE id = ?`, code, h.ID); err != nil {
			errs = append(errs, fmt.Sprintf("hook %d record exit: %v", h.ID, err))
		}
		fired++
	}

	// Now sweep. Anything still pending had its chance above and had no match
	// inside its own window, so it can never fire.
	if r, err := db.ExecContext(ctx,
		`UPDATE event_hooks SET status = ? WHERE status = ? AND expires_ms <= ?`,
		HookExpired, HookPending, nowMS); err == nil {
		n, _ := r.RowsAffected()
		expired = int(n)
	} else {
		errs = append(errs, fmt.Sprintf("expire hooks: %v", err))
	}
	return fired, expired, errs
}

// listPendingRegardlessOfExpiry is deliberately not ListHooks(pendingOnly):
// a hook past its expiry can still be owed a fire for an event that landed
// while it was live.
func listPendingRegardlessOfExpiry(ctx context.Context, db *sql.DB) ([]Hook, error) {
	all, err := ListHooks(ctx, db, time.Time{}, false)
	if err != nil {
		return nil, err
	}
	out := make([]Hook, 0, len(all))
	for _, h := range all {
		if h.Status == HookPending {
			out = append(out, h)
		}
	}
	return out, nil
}

// PruneHooks deletes finished one-shots older than cutoff. Fired rows are kept
// for a while so `when --list --all` can show what happened, but not forever.
func PruneHooks(ctx context.Context, db *sql.DB, cutoffMS int64) (int64, error) {
	r, err := db.ExecContext(ctx,
		`DELETE FROM event_hooks WHERE status != ? AND created_ms < ?`, HookPending, cutoffMS)
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	return n, nil
}

func tailOf(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

func claimHook(ctx context.Context, db *sql.DB, hookID, eventID, nowMS int64) (bool, error) {
	r, err := db.ExecContext(ctx, `
		UPDATE event_hooks
		   SET status = ?, fired_ms = ?, fired_event_id = ?
		 WHERE id = ? AND status = ?`,
		HookFired, nowMS, eventID, hookID, HookPending)
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n > 0, nil
}

// execHook runs the command with the event delivered on stdin as JSON.
//
// The event never reaches the command through its argument string. A project
// path containing a quote would otherwise be a shell injection, and detail
// values are attacker-adjacent in the sense that they come from a scraped
// terminal panel.
func execHook(ctx context.Context, h Hook, ev Event, now time.Time) (int, []byte, error) {
	// Detached from the caller's cancellation, and bounded only by
	// HookTimeout. The hook has already been claimed, so it will never be
	// retried; letting a daemon SIGTERM or the tail of a tight poll context
	// kill it would consume the one shot without running it. The timeout still
	// caps how long a hanging handler can hold the reconciler.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), HookTimeout)
	defer cancel()

	shell, flag := "sh", "-c"
	if runtime.GOOS == "windows" {
		shell, flag = "cmd", "/c"
	}
	cmd := exec.CommandContext(runCtx, shell, flag, h.Command)

	payload, err := json.Marshal(ev)
	if err != nil {
		return -1, nil, err
	}
	cmd.Stdin = bytes.NewReader(payload)

	// LateS is how long after the event the fire actually happened. It is what
	// lets a handler tell a normal fire from a suspend-recovery fire hours
	// after the fact, which a naive "it just ran" design would lose.
	lateS := (now.UnixMilli() - ev.TSUnixMS) / 1000
	cmd.Env = append(os.Environ(),
		"BLOODHOUND_EVENT_ID="+strconv.FormatInt(ev.ID, 10),
		"BLOODHOUND_EVENT_KIND="+ev.Kind,
		"BLOODHOUND_EVENT_BUCKET="+ev.Scope.Bucket,
		"BLOODHOUND_EVENT_SESSION="+ev.Scope.Session,
		"BLOODHOUND_EVENT_CWD="+ev.Scope.Cwd,
		"BLOODHOUND_EVENT_LATE_S="+strconv.FormatInt(lateS, 10),
		"BLOODHOUND_HOOK_ID="+strconv.FormatInt(h.ID, 10),
	)

	output, err := cmd.CombinedOutput()
	if err == nil {
		return 0, output, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), output, nil
	}
	return -1, output, err
}
