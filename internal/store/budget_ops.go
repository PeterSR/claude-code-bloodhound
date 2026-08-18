package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/budget"
	"github.com/PeterSR/claude-code-bloodhound/internal/events"
)

// Budget lifecycle events.
//
// Pressure is a level, diffed by the reconciler, and surfaces as
// budget.clear / budget.tight / budget.exceeded. Lifecycle is not a level: a
// budget being set is an edge with no persistent state, so it is appended by
// whatever transaction discovered it, the same way window.reset is. That also
// means it works on a cron install with no daemon, since `bloodhound budget
// set` goes through exactly this path.
//
// They deliberately share the "budget" prefix. The state names (clear, tight,
// exceeded) and the lifecycle verbs (set, revoked, expired) cannot collide, so
// a peer wanting everything budget-related globs budget.* and gets both, while
// one wanting only pressure can still name the states.
const (
	EventBudgetSet     = "budget.set"
	EventBudgetRevoked = "budget.revoked"
	EventBudgetExpired = "budget.expired"
)

// SetBudget stores a budget for a directory and bucket, retiring whatever live
// budget was there before.
//
// Replacing rather than editing is deliberate: the previous allowance is kept
// as a retired row so `budget list --all` can say what it was, and so that
// re-setting a budget mid-window reads as a new decision rather than a silent
// rewrite of one already in force.
func (s *Store) SetBudget(ctx context.Context, b budget.Budget, now time.Time) (budget.Budget, error) {
	if err := budget.Validate(b); err != nil {
		return budget.Budget{}, err
	}
	nowMS := now.UnixMilli()

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return budget.Budget{}, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`UPDATE budgets SET retired_ms = ?, retired_why = ?
		   WHERE cwd = ? AND bucket = ? AND retired_ms IS NULL`,
		nowMS, budget.RetiredReplaced, b.Cwd, b.Bucket)
	if err != nil {
		return budget.Budget{}, fmt.Errorf("retire previous budget: %w", err)
	}
	displaced, err := res.RowsAffected()
	if err != nil {
		return budget.Budget{}, err
	}
	replaced := displaced > 0

	ins, err := tx.ExecContext(ctx,
		`INSERT INTO budgets (cwd, bucket, spend_pct, meter_pct, note, set_ms)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		b.Cwd, b.Bucket, b.SpendPct, b.MeterPct, b.Note, nowMS)
	if err != nil {
		return budget.Budget{}, fmt.Errorf("insert budget: %w", err)
	}
	id, err := ins.LastInsertId()
	if err != nil {
		return budget.Budget{}, err
	}
	b.ID, b.SetMS = id, nowMS

	for i := range b.Leases {
		if err := insertLease(ctx, tx, id, &b.Leases[i]); err != nil {
			return budget.Budget{}, err
		}
	}

	// One event for the whole change, carrying whether it displaced something.
	// A replacement is not also reported as a revoke: a peer watching
	// budget.revoked wants "this directory is no longer governed", which a
	// replacement is precisely not.
	detail := map[string]any{"replaced": replaced}
	if b.HasSpend() {
		detail["spend_pct"] = b.SpendPct
	}
	if b.HasMeter() {
		detail["meter_pct"] = b.MeterPct
	}
	if b.Note != "" {
		detail["note"] = b.Note
	}
	if _, err := events.AppendTx(ctx, tx, nowMS, EventBudgetSet,
		events.Scope{Bucket: b.Bucket, Cwd: b.Cwd}, detail); err != nil {
		return budget.Budget{}, fmt.Errorf("append budget.set: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return budget.Budget{}, err
	}
	return b, nil
}

func insertLease(ctx context.Context, tx *sql.Tx, budgetID int64, l *budget.Lease) error {
	res, err := tx.ExecContext(ctx,
		`INSERT INTO budget_leases (budget_id, kind, at_ms, bucket, bound_after_ms, window_end_ms, expired_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		budgetID, l.Kind, l.AtMS, l.Bucket, l.BoundAfterMS, l.WindowEndMS, l.ExpiredMS)
	if err != nil {
		return fmt.Errorf("insert lease: %w", err)
	}
	l.ID, err = res.LastInsertId()
	return err
}

// RevokeBudget retires the live budget for a directory and bucket. It reports
// whether there was one to retire.
func (s *Store) RevokeBudget(ctx context.Context, cwd, bucket string, now time.Time) (bool, error) {
	nowMS := now.UnixMilli()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`UPDATE budgets SET retired_ms = ?, retired_why = ?
		   WHERE cwd = ? AND bucket = ? AND retired_ms IS NULL`,
		nowMS, budget.RetiredRevoked, cwd, bucket)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		// Nothing was in force, so nothing happened. Emitting an event here
		// would tell a peer a budget ended when none existed.
		return false, tx.Commit()
	}

	// The level the reconciler recorded for this budget is now about something
	// that no longer exists. Clearing it in the same transaction stops
	// `budget status` reporting pressure for a revoked budget until the next
	// tick, and stops the next reconcile reading a stale previous state.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM event_levels WHERE kind = 'budget' AND bucket = ? AND cwd = ?`,
		bucket, cwd); err != nil {
		return false, fmt.Errorf("clear budget level: %w", err)
	}
	if _, err := events.AppendTx(ctx, tx, nowMS, EventBudgetRevoked,
		events.Scope{Bucket: bucket, Cwd: cwd}, nil); err != nil {
		return false, fmt.Errorf("append budget.revoked: %w", err)
	}
	return true, tx.Commit()
}

// ListBudgets returns budgets with their leases. Live-only unless includeRetired.
func (s *Store) ListBudgets(ctx context.Context, includeRetired bool) ([]budget.Budget, error) {
	q := `SELECT id, cwd, bucket, spend_pct, meter_pct, note, set_ms, retired_ms, retired_why
	        FROM budgets`
	if !includeRetired {
		q += ` WHERE retired_ms IS NULL`
	}
	q += ` ORDER BY cwd, bucket, id DESC`

	rows, err := s.DB.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []budget.Budget
	byID := map[int64]int{}
	for rows.Next() {
		var b budget.Budget
		var retiredMS sql.NullInt64
		if err := rows.Scan(&b.ID, &b.Cwd, &b.Bucket, &b.SpendPct, &b.MeterPct,
			&b.Note, &b.SetMS, &retiredMS, &b.RetiredWhy); err != nil {
			return nil, err
		}
		if retiredMS.Valid {
			v := retiredMS.Int64
			b.RetiredMS = &v
		}
		byID[b.ID] = len(out)
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	if err := s.loadLeases(ctx, out, byID); err != nil {
		return nil, err
	}
	budget.SortBudgets(out)
	return out, nil
}

func (s *Store) loadLeases(ctx context.Context, budgets []budget.Budget, byID map[int64]int) error {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, budget_id, kind, at_ms, bucket, bound_after_ms, window_end_ms, expired_ms
		   FROM budget_leases ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			l                    budget.Lease
			budgetID             int64
			at, bound, wend, exp sql.NullInt64
		)
		if err := rows.Scan(&l.ID, &budgetID, &l.Kind, &at, &l.Bucket, &bound, &wend, &exp); err != nil {
			return err
		}
		idx, ok := byID[budgetID]
		if !ok {
			continue // lease for a budget outside this listing
		}
		l.AtMS = nullInt64Ptr(at)
		l.BoundAfterMS = nullInt64Ptr(bound)
		l.WindowEndMS = nullInt64Ptr(wend)
		l.ExpiredMS = nullInt64Ptr(exp)
		budgets[idx].Leases = append(budgets[idx].Leases, l)
	}
	return rows.Err()
}

func nullInt64Ptr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

// SettleBudgets expires run-out leases and retires any budget whose last lease
// has gone. Returns the budgets still in force.
//
// This is the only writer of retired_why = "expired", and it runs from the
// daemon's reconcile pass so a budget dies on schedule rather than the next
// time somebody happens to look at it.
func (s *Store) SettleBudgets(ctx context.Context, now time.Time, windowEnd map[string]int64) ([]budget.Budget, error) {
	live, err := s.ListBudgets(ctx, false)
	if err != nil {
		return nil, err
	}
	nowMS := now.UnixMilli()
	var still []budget.Budget

	for i := range live {
		b := &live[i]
		before := expiredIDs(*b)
		alive := budget.SettleLeases(b, now, windowEnd)

		// One transaction per budget. Everything that ends a budget has to
		// land together or not at all: without it, a crash between the retire
		// and the append loses budget.expired permanently, because the budget
		// is already retired and no later pass rediscovers it. The conditional
		// UPDATE on retired_ms is also what makes this safe to run
		// concurrently, since only one settler can retire a given budget and
		// therefore only one can append its event.
		if err := s.settleOne(ctx, b, before, alive, nowMS); err != nil {
			return nil, err
		}
		if alive {
			still = append(still, *b)
		}
	}
	return still, nil
}

func (s *Store) settleOne(ctx context.Context, b *budget.Budget, before map[int64]bool, alive bool, nowMS int64) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, l := range b.Leases {
		if l.ExpiredMS != nil && !before[l.ID] {
			if _, err := tx.ExecContext(ctx,
				`UPDATE budget_leases SET expired_ms = ? WHERE id = ? AND expired_ms IS NULL`,
				*l.ExpiredMS, l.ID); err != nil {
				return fmt.Errorf("expire lease %d: %w", l.ID, err)
			}
		}
	}
	if alive {
		return tx.Commit()
	}

	// Guarded by retired_ms IS NULL so a concurrent settler that got here
	// first takes the retirement, and this one appends nothing.
	res, err := tx.ExecContext(ctx,
		`UPDATE budgets SET retired_ms = ?, retired_why = ? WHERE id = ? AND retired_ms IS NULL`,
		nowMS, budget.RetiredExpired, b.ID)
	if err != nil {
		return fmt.Errorf("retire budget %d: %w", b.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return tx.Commit() // somebody else retired it; their event stands
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM event_levels WHERE kind = 'budget' AND bucket = ? AND cwd = ?`,
		b.Bucket, b.Cwd); err != nil {
		return fmt.Errorf("clear level for budget %d: %w", b.ID, err)
	}
	if _, err := events.AppendTx(ctx, tx, nowMS, EventBudgetExpired,
		events.Scope{Bucket: b.Bucket, Cwd: b.Cwd},
		map[string]any{"set_ms": b.SetMS}); err != nil {
		return fmt.Errorf("append budget.expired: %w", err)
	}
	return tx.Commit()
}

// PruneOrphanBudgetLevels deletes recorded pressure for (cwd, bucket) pairs
// that no longer have a live budget.
//
// Deleting the level when a budget is revoked or expires is not enough on its
// own. Reconcile collects its sensor readings BEFORE opening the transaction
// that writes them, so a pass that read a budget as live can upsert its level
// after the revoke committed, and nothing else would ever remove it:
// PruneLevels only touches rows with a non-empty session, which a budget level
// never has. A resurrected row is not merely cosmetic — re-setting the same
// budget later would find the level already in that state, see no transition,
// and silently swallow the warning.
//
// Running this at the top of every sensor pass bounds the damage to a single
// tick instead of forever.
func (s *Store) PruneOrphanBudgetLevels(ctx context.Context) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `
		DELETE FROM event_levels
		 WHERE kind = 'budget'
		   AND NOT EXISTS (
		         SELECT 1 FROM budgets
		          WHERE budgets.cwd = event_levels.cwd
		            AND budgets.bucket = event_levels.bucket
		            AND budgets.retired_ms IS NULL)`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func expiredIDs(b budget.Budget) map[int64]bool {
	m := make(map[int64]bool, len(b.Leases))
	for _, l := range b.Leases {
		if l.ExpiredMS != nil {
			m[l.ID] = true
		}
	}
	return m
}

// GetMeta reads a meta key, returning "" when it is not set.
func (s *Store) GetMeta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.DB.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return v, nil
}

// SetMeta writes a meta key.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
