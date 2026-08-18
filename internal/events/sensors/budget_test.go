package sensors

import (
	"context"
	"testing"
	"time"

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

	rs, err := readBudgetPressure(ctx, events.World{DB: s.DB, Now: now})
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
	rs, err := readBudgetPressure(ctx, events.World{DB: s.DB, Now: time.Now()})
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
	rs, err := readBudgetPressure(ctx, events.World{DB: s.DB, Now: now})
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

	rs, err := readBudgetPressure(ctx, events.World{DB: s.DB, Now: now})
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
