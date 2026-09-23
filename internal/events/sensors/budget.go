package sensors

import (
	"context"
	"fmt"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
	"github.com/PeterSR/claude-code-bloodhound/internal/budget"
	"github.com/PeterSR/claude-code-bloodhound/internal/events"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

// BudgetWindows is how far back the sensor looks for a bucket's open window.
// Doubled past each bucket's real span, matching the slack `attribution
// --window current` already allows, so an unusually long window or a late
// aggregator pass still finds it.
var budgetLookback = map[string]time.Duration{
	budget.BucketSession: 10 * time.Hour,
	budget.BucketWeek:    14 * 24 * time.Hour,
}

func init() {
	// Budget pressure, one level per directory and bucket.
	//
	// This is the only sensor scoped to a working directory, and the only one
	// whose readings depend on user configuration rather than on the meter
	// alone. A directory with no budget produces no reading at all, rather
	// than a "clear" one: nothing was asked, so there is nothing to answer,
	// and emitting clear would put every directory that ever ran in the log.
	//
	// Deliberately absent: any second opinion on the 5h cliff. That is what
	// limit_projection, threshold and saturation already are, and they fire
	// whether or not a directory has a budget. The governor this came from
	// carried its own wall guard because it had no sensors of its own.
	events.Register(events.Sensor{
		Name: "budget/pressure",
		Read: readBudgetPressure,
	})
}

func readBudgetPressure(ctx context.Context, w events.World) ([]events.Reading, error) {
	// Same bail as every other meter sensor, and for the same reason its
	// comment gives: a transient failure to compute the pool says nothing
	// about any budget, and reporting unknown would flip a meter-rule level to
	// unknown and straight back on the next tick, consuming any one-shot
	// watching budget.* and re-announcing on recovery. Spend-rule budgets read
	// from the database and would survive, but reporting half the budgets on a
	// bad tick is worse than reporting none.
	if w.Pool == nil {
		return nil, nil
	}

	s := &store.Store{DB: w.DB}

	// Clear pressure left behind by budgets that are already gone before
	// reading anything, so a level resurrected by a racing pass lives one tick
	// rather than forever. See PruneOrphanBudgetLevels.
	if _, err := s.PruneOrphanBudgetLevels(ctx); err != nil {
		return nil, fmt.Errorf("prune orphan budget levels: %w", err)
	}

	// A budget is measured on the meter of the account its directory spends
	// on: the account of the directory's newest turn. Everything below is
	// computed per account, once, and shared by that account's budgets.
	accountOf := map[string]int64{}
	var lookupErr error
	accountFor := func(cwd string) int64 {
		if id, ok := accountOf[cwd]; ok {
			return id
		}
		id, err := s.AccountForCwd(ctx, cwd)
		if err != nil && lookupErr == nil {
			lookupErr = err
		}
		accountOf[cwd] = id
		return id
	}
	endsOf := map[int64]map[string]int64{}
	endsFor := func(acct int64) map[string]int64 {
		if m, ok := endsOf[acct]; ok {
			return m
		}
		m, err := currentWindowEnds(ctx, s, acct, w.Now)
		if err != nil && lookupErr == nil {
			lookupErr = err
		}
		endsOf[acct] = m
		return m
	}

	// Settling first means a budget whose last lease ran out stops producing
	// readings on the same tick it dies, rather than one tick later.
	live, err := s.SettleBudgets(ctx, w.Now, func(b budget.Budget) map[string]int64 {
		return endsFor(accountFor(b.Cwd))
	})
	if err != nil {
		return nil, fmt.Errorf("settle budgets: %w", err)
	}
	if lookupErr != nil {
		return nil, lookupErr
	}
	if len(live) == 0 {
		return nil, nil
	}

	pools := map[int64]*routes.NowResponse{}
	for _, m := range w.Meters {
		pools[m.Account] = m.Pool
	}

	// Attribution per directory, computed once per (account, bucket) rather
	// than once per budget: several directories usually share a bucket and
	// the query is the expensive part.
	// measured records that the bucket's open window WAS queried, separately
	// from what the query returned. A nil map and an absent key are both falsy
	// on lookup, so without this a bucket with no open window would read as
	// "measured, zero spent" and every spend rule would report clear off a
	// measurement that never happened.
	type acctBucket struct {
		account int64
		bucket  string
	}
	attributed := map[acctBucket]map[string]float64{}
	measured := map[acctBucket]bool{}
	for _, b := range live {
		k := acctBucket{accountFor(b.Cwd), b.Bucket}
		if _, done := measured[k]; done {
			continue
		}
		byCwd, ok, err := currentWindowByCwd(ctx, s, k.account, k.bucket, w.Now)
		if err != nil {
			return nil, err
		}
		attributed[k], measured[k] = byCwd, ok
	}

	var out []events.Reading
	for _, b := range live {
		acct := accountFor(b.Cwd)
		k := acctBucket{acct, b.Bucket}
		win := budget.Window{}
		// meter is the bucket's live window, kept past the read because the
		// reading wants its reset as well as its percentage. Its reset is the
		// one /usage shows the user, which is not always the millisecond
		// windowEnds derives from the stored limit windows below; the message
		// should agree with the panel the reader can go and look at.
		var meter *routes.NowWindow
		if pool := pools[acct]; pool != nil {
			meter = buckets(pool)[b.Bucket]
		}
		if meter != nil {
			win.Pct, win.PctKnown = meter.Pct, true
		}
		if measured[k] {
			// A directory absent from the rollup caused no measured movement
			// in this window, which is a real zero rather than a gap: the
			// query covered the window, the directory simply is not in it.
			win.AttributedPct, win.AttributedKnown = attributed[k][b.Cwd], true
		}
		win.EndMS = endsFor(acct)[b.Bucket]

		p := budget.Evaluate(b, win)
		r := events.Reading{
			Kind:  "budget",
			Scope: events.Scope{Bucket: b.Bucket, Cwd: b.Cwd},
			State: p.State,
		}
		if p.State != "" {
			r.Detail = map[string]any{
				"rule":      p.Rule,
				"reason":    p.Reason,
				"meter_pct": p.MeterPct,
			}
			if b.HasSpend() {
				r.Detail["spend_pct"] = b.SpendPct
				r.Detail["spent_pct"] = p.SpentPct
				r.Detail["remaining_pct"] = p.RemainingPct
			}
			if b.HasMeter() {
				r.Detail["meter_stop_pct"] = b.MeterPct
			}
		}
		withReset(&r, meter)
		out = append(out, r)
	}
	return out, nil
}

func bucketWanted(bs []budget.Budget, bucket string) bool {
	for _, b := range bs {
		if b.Bucket == bucket {
			return true
		}
	}
	return false
}

// currentWindowEnds reports the end of each bucket's most recent window, keyed
// by the budget spelling of the bucket. A bucket with no window at all in range
// is absent rather than zero, which is what a window_reset lease needs to tell
// "nothing to attach to yet" from "already over".
//
// The open window is preferred, but a bucket with none falls back to the last
// window that CLOSED. Without that fallback a window which opened and shut
// while nobody was watching (laptop suspended, daemon down) is invisible to
// the lease, which then silently rolls to the end of the next window: up to
// five hours late for the session bucket and a week for the weekly one. The
// closed rows were in the store the whole time; the lease just never looked.
func currentWindowEnds(ctx context.Context, s *store.Store, accountID int64, now time.Time) (map[string]int64, error) {
	out := map[string]int64{}
	for _, bucket := range []string{budget.BucketSession, budget.BucketWeek} {
		since := now.Add(-budgetLookback[bucket]).UnixMilli()
		windows, err := s.ListLimitWindows(ctx, accountID, budget.AttributeBucket(bucket), since)
		if err != nil {
			return nil, fmt.Errorf("list %s windows: %w", bucket, err)
		}
		// Oldest first, so the last entry is the most recent window whether or
		// not it is still open.
		if n := len(windows); n > 0 {
			out[bucket] = windows[n-1].ResetUnixMS
		}
	}
	return out, nil
}

func currentWindow(ctx context.Context, s *store.Store, accountID int64, bucket string, now time.Time) (*store.LimitWindowRow, error) {
	since := now.Add(-budgetLookback[bucket]).UnixMilli()
	windows, err := s.ListLimitWindows(ctx, accountID, budget.AttributeBucket(bucket), since)
	if err != nil {
		return nil, fmt.Errorf("list %s windows: %w", bucket, err)
	}
	// Oldest first, so the open one is last when there is one.
	for i := len(windows) - 1; i >= 0; i-- {
		if windows[i].InProgress {
			return &windows[i], nil
		}
	}
	return nil, nil
}

// currentWindowByCwd is each directory's attributed share of the bucket's open
// window. The bool reports whether there was a window to measure at all, which
// the caller must not confuse with the map being empty: no open window means a
// spend rule cannot be evaluated, while an empty map means it can and nobody
// has spent anything.
func currentWindowByCwd(ctx context.Context, s *store.Store, accountID int64, bucket string, now time.Time) (map[string]float64, bool, error) {
	win, err := currentWindow(ctx, s, accountID, bucket, now)
	if err != nil {
		return nil, false, err
	}
	if win == nil {
		return nil, false, nil
	}
	// Since the window's own start, so only this window is in range. That is
	// the same narrowing `attribution --window current` performs, and the
	// reason a budget can be denominated in "percent of one window" at all.
	groups, err := s.GroupAttribution(ctx, accountID, budget.AttributeBucket(bucket), "cwd", win.StartUnixMS)
	if err != nil {
		return nil, false, fmt.Errorf("attribution by cwd for %s: %w", bucket, err)
	}
	out := make(map[string]float64, len(groups))
	for _, g := range groups {
		if g.Key == "" || g.Key == store.UnknownCwd {
			continue // unattributed movement, and spend with no directory, belong to nobody
		}
		out[g.Key] = g.Pct
	}
	return out, true, nil
}
