package sensors

import (
	"context"
	"testing"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
	"github.com/PeterSR/claude-code-bloodhound/internal/budget"
	"github.com/PeterSR/claude-code-bloodhound/internal/events"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

func sensorStore(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	ctx := context.Background()
	s, err := store.Open(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, ctx
}

// A pool with both windows present but no readings in them. The sensor bails
// entirely on a nil pool (a transient compute failure says nothing about any
// budget), so every test that wants to exercise the budget path has to supply
// one, and nil-pool behaviour gets its own test below.
func emptyPool() *routes.NowResponse {
	return &routes.NowResponse{OK: true}
}

func readingFor(rs []events.Reading, bucket string) (events.Reading, bool) {
	for _, r := range rs {
		if r.Scope.Bucket == bucket {
			return r, true
		}
	}
	return events.Reading{}, false
}

// The bug this pins: currentWindowByCwd returns a nil map when no limit window
// is open, and a nil map is indistinguishable from an empty one on lookup. The
// first version therefore read "measured, zero spent" and reported every spend
// rule as clear off a measurement that had never happened, which is precisely
// the invented headroom a budget exists to prevent.
func TestBudgetSensorReportsUnknownWithNoOpenWindow(t *testing.T) {
	s, ctx := sensorStore(t)
	now := time.Now()

	if _, err := s.SetBudget(ctx, budget.Budget{
		Cwd: "/home/dev/myapp", Bucket: budget.BucketWeek, SpendPct: 25,
		Leases: []budget.Lease{{Kind: budget.LeaseManual}},
	}, now); err != nil {
		t.Fatal(err)
	}

	rs, err := readBudgetPressure(ctx, events.World{DB: s.DB, Now: now, Pool: emptyPool()})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	r, ok := readingFor(rs, budget.BucketWeek)
	if !ok {
		t.Fatal("no reading for the week bucket")
	}
	if r.State != "" {
		t.Errorf("state = %q, want empty (unknown): there is no window to measure against", r.State)
	}
	if r.Scope.Cwd != "/home/dev/myapp" {
		t.Errorf("scope cwd = %q, want the budget's directory", r.Scope.Cwd)
	}
}

func TestBudgetSensorIsSilentWithNoBudgets(t *testing.T) {
	// A directory with no budget asked nothing, so there is nothing to answer.
	// Emitting "clear" would put every directory that ever ran into the log.
	s, ctx := sensorStore(t)
	rs, err := readBudgetPressure(ctx, events.World{DB: s.DB, Now: time.Now(), Pool: emptyPool()})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(rs) != 0 {
		t.Errorf("got %d readings with no budgets set, want 0", len(rs))
	}
}

func TestBudgetSensorScopesEachBudgetToItsOwnDirectory(t *testing.T) {
	s, ctx := sensorStore(t)
	now := time.Now()
	for _, cwd := range []string{"/home/dev/one", "/home/dev/two"} {
		if _, err := s.SetBudget(ctx, budget.Budget{
			Cwd: cwd, Bucket: budget.BucketWeek, SpendPct: 10,
			Leases: []budget.Lease{{Kind: budget.LeaseManual}},
		}, now); err != nil {
			t.Fatal(err)
		}
	}
	rs, err := readBudgetPressure(ctx, events.World{DB: s.DB, Now: now, Pool: emptyPool()})
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 {
		t.Fatalf("got %d readings, want one per directory", len(rs))
	}
	seen := map[string]bool{}
	for _, r := range rs {
		if r.Kind != "budget" {
			t.Errorf("kind = %q, want budget", r.Kind)
		}
		seen[r.Scope.Cwd] = true
	}
	for _, cwd := range []string{"/home/dev/one", "/home/dev/two"} {
		if !seen[cwd] {
			t.Errorf("no reading scoped to %s", cwd)
		}
	}
}

func TestBudgetSensorSettlesExpiredBudgetsBeforeReporting(t *testing.T) {
	// A budget whose last lease ran out must stop producing readings on the
	// same tick it dies, not one tick later.
	s, ctx := sensorStore(t)
	now := time.Now()
	past := now.Add(-time.Hour).UnixMilli()

	if _, err := s.SetBudget(ctx, budget.Budget{
		Cwd: "/home/dev/myapp", Bucket: budget.BucketWeek, SpendPct: 10,
		Leases: []budget.Lease{{Kind: budget.LeaseDeadline, AtMS: &past}},
	}, now); err != nil {
		t.Fatal(err)
	}

	rs, err := readBudgetPressure(ctx, events.World{DB: s.DB, Now: now, Pool: emptyPool()})
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 0 {
		t.Errorf("got %d readings from an expired budget, want 0", len(rs))
	}
	all, _ := s.ListBudgets(ctx, true)
	if len(all) != 1 || all[0].RetiredMS == nil {
		t.Error("the expired budget was not retired by the sensor pass")
	}
}

// Matches every other meter sensor, and the reason its comment gives: treating
// a transient failure to compute the pool as "actively unknown" would flip a
// meter-rule level to unknown and straight back on the next tick, consuming
// any one-shot watching budget.* and re-announcing on recovery.
func TestBudgetSensorSaysNothingWithoutAPool(t *testing.T) {
	s, ctx := sensorStore(t)
	now := time.Now()
	if _, err := s.SetBudget(ctx, budget.Budget{
		Cwd: "/home/dev/myapp", Bucket: budget.BucketWeek, MeterPct: 70,
		Leases: []budget.Lease{{Kind: budget.LeaseManual}},
	}, now); err != nil {
		t.Fatal(err)
	}
	rs, err := readBudgetPressure(ctx, events.World{DB: s.DB, Now: now, Pool: nil})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(rs) != 0 {
		t.Errorf("got %d readings with no pool, want 0", len(rs))
	}
}

// A level left behind by a budget that is already gone must not survive. The
// reconciler collects sensor readings before opening its write transaction, so
// a racing pass can upsert a level after a revoke deleted it, and nothing else
// would ever remove it: PruneLevels only touches rows with a session. A
// resurrected row is not cosmetic, because re-setting the same budget would
// find the level already in that state, see no transition, and swallow the
// warning.
func TestBudgetSensorClearsLevelsLeftByDepartedBudgets(t *testing.T) {
	s, ctx := sensorStore(t)
	now := time.Now()

	// Stand in for a level a racing reconcile wrote after the budget went.
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO event_levels (kind, bucket, session_uuid, cwd, state, since_ms)
		 VALUES ('budget', ?, '', ?, 'tight', ?)`,
		budget.BucketWeek, "/home/dev/ghost", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}

	if _, err := readBudgetPressure(ctx, events.World{DB: s.DB, Now: now, Pool: emptyPool()}); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT count(*) FROM event_levels WHERE kind = 'budget' AND cwd = ?`,
		"/home/dev/ghost").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("an orphaned budget level survived a sensor pass")
	}
}

// And a level belonging to a budget that IS live must be left alone, or the
// prune would delete the pressure it just recorded.
func TestBudgetSensorKeepsLevelsForLiveBudgets(t *testing.T) {
	s, ctx := sensorStore(t)
	now := time.Now()
	if _, err := s.SetBudget(ctx, budget.Budget{
		Cwd: "/home/dev/myapp", Bucket: budget.BucketWeek, SpendPct: 10,
		Leases: []budget.Lease{{Kind: budget.LeaseManual}},
	}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO event_levels (kind, bucket, session_uuid, cwd, state, since_ms)
		 VALUES ('budget', ?, '', ?, 'tight', ?)`,
		budget.BucketWeek, "/home/dev/myapp", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := readBudgetPressure(ctx, events.World{DB: s.DB, Now: now, Pool: emptyPool()}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT count(*) FROM event_levels WHERE kind = 'budget' AND cwd = ?`,
		"/home/dev/myapp").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("the prune deleted a live budget's own level")
	}
}
