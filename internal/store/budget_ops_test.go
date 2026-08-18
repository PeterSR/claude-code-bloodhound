package store

import (
	"context"
	"testing"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/budget"
)

func budgetStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	ctx := context.Background()
	s, err := Open(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, ctx
}

func manualBudget(cwd, bucket string, spend float64) budget.Budget {
	return budget.Budget{
		Cwd:      cwd,
		Bucket:   bucket,
		SpendPct: spend,
		Leases:   []budget.Lease{{Kind: budget.LeaseManual}},
	}
}

func TestSetBudgetRoundTrips(t *testing.T) {
	s, ctx := budgetStore(t)
	now := time.Now()

	saved, err := s.SetBudget(ctx, manualBudget("/home/dev/myapp", budget.BucketWeek, 25), now)
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if saved.ID == 0 {
		t.Fatal("no id assigned")
	}

	got, err := s.ListBudgets(ctx, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d budgets, want 1", len(got))
	}
	if got[0].Cwd != "/home/dev/myapp" || got[0].SpendPct != 25 {
		t.Errorf("round trip lost fields: %+v", got[0])
	}
	if len(got[0].Leases) != 1 || got[0].Leases[0].Kind != budget.LeaseManual {
		t.Errorf("leases = %+v, want one manual", got[0].Leases)
	}
	if !got[0].Live() {
		t.Error("a budget with a manual lease is not live")
	}
}

func TestSetBudgetRetiresThePreviousOne(t *testing.T) {
	// Replacing rather than editing: the old allowance stays readable, and
	// only one budget per directory and bucket is ever in force.
	s, ctx := budgetStore(t)
	now := time.Now()

	if _, err := s.SetBudget(ctx, manualBudget("/home/dev/myapp", budget.BucketWeek, 10), now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetBudget(ctx, manualBudget("/home/dev/myapp", budget.BucketWeek, 40), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	live, err := s.ListBudgets(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 {
		t.Fatalf("got %d live budgets, want 1", len(live))
	}
	if live[0].SpendPct != 40 {
		t.Errorf("live budget is %v%%, want the newest (40)", live[0].SpendPct)
	}

	all, err := s.ListBudgets(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d budgets with --all, want 2: the old one must survive as history", len(all))
	}
}

func TestBudgetsInDifferentDirectoriesCoexist(t *testing.T) {
	s, ctx := budgetStore(t)
	now := time.Now()
	for _, cwd := range []string{"/home/dev/one", "/home/dev/two"} {
		if _, err := s.SetBudget(ctx, manualBudget(cwd, budget.BucketWeek, 10), now); err != nil {
			t.Fatal(err)
		}
	}
	// Same directory, other bucket: also independent.
	if _, err := s.SetBudget(ctx, manualBudget("/home/dev/one", budget.BucketSession, 5), now); err != nil {
		t.Fatal(err)
	}
	live, err := s.ListBudgets(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 3 {
		t.Fatalf("got %d live budgets, want 3", len(live))
	}
}

func TestRevokeBudget(t *testing.T) {
	s, ctx := budgetStore(t)
	now := time.Now()
	if _, err := s.SetBudget(ctx, manualBudget("/home/dev/myapp", budget.BucketWeek, 10), now); err != nil {
		t.Fatal(err)
	}

	had, err := s.RevokeBudget(ctx, "/home/dev/myapp", budget.BucketWeek, now)
	if err != nil {
		t.Fatal(err)
	}
	if !had {
		t.Fatal("revoke reported nothing to revoke")
	}
	live, _ := s.ListBudgets(ctx, false)
	if len(live) != 0 {
		t.Errorf("got %d live budgets after revoke, want 0", len(live))
	}

	// Revoking again is not an error, it just reports there was nothing.
	had, err = s.RevokeBudget(ctx, "/home/dev/myapp", budget.BucketWeek, now)
	if err != nil {
		t.Fatal(err)
	}
	if had {
		t.Error("second revoke claimed to retire something")
	}
}

func TestSettleBudgetsRetiresExpiredAndKeepsTheRest(t *testing.T) {
	s, ctx := budgetStore(t)
	now := time.Now()
	past := now.Add(-time.Hour).UnixMilli()

	// One with a deadline that has passed, one perpetual.
	dying := budget.Budget{
		Cwd: "/home/dev/dying", Bucket: budget.BucketWeek, SpendPct: 10,
		Leases: []budget.Lease{{Kind: budget.LeaseDeadline, AtMS: &past}},
	}
	if _, err := s.SetBudget(ctx, dying, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetBudget(ctx, manualBudget("/home/dev/living", budget.BucketWeek, 10), now); err != nil {
		t.Fatal(err)
	}

	still, err := s.SettleBudgets(ctx, now, nil)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if len(still) != 1 || still[0].Cwd != "/home/dev/living" {
		t.Fatalf("survivors = %+v, want only /home/dev/living", still)
	}

	all, _ := s.ListBudgets(ctx, true)
	for _, b := range all {
		if b.Cwd == "/home/dev/dying" {
			if b.RetiredMS == nil {
				t.Error("the expired budget was not retired")
			}
			if b.RetiredWhy != budget.RetiredExpired {
				t.Errorf("retired_why = %q, want %q", b.RetiredWhy, budget.RetiredExpired)
			}
		}
	}
}

func TestSettleBudgetsIsIdempotent(t *testing.T) {
	// It runs on every reconcile tick, so a second pass over the same state
	// must not churn rows or re-retire anything.
	s, ctx := budgetStore(t)
	now := time.Now()
	past := now.Add(-time.Hour).UnixMilli()
	if _, err := s.SetBudget(ctx, budget.Budget{
		Cwd: "/home/dev/myapp", Bucket: budget.BucketWeek, SpendPct: 10,
		Leases: []budget.Lease{{Kind: budget.LeaseDeadline, AtMS: &past}},
	}, now); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		still, err := s.SettleBudgets(ctx, now.Add(time.Duration(i)*time.Minute), nil)
		if err != nil {
			t.Fatalf("settle pass %d: %v", i, err)
		}
		if len(still) != 0 {
			t.Fatalf("pass %d: %d survivors, want 0", i, len(still))
		}
	}
	all, _ := s.ListBudgets(ctx, true)
	if len(all) != 1 {
		t.Errorf("got %d rows, want 1: settling must not duplicate", len(all))
	}
}

func TestSetBudgetRejectsAnInvalidOne(t *testing.T) {
	s, ctx := budgetStore(t)
	_, err := s.SetBudget(ctx, budget.Budget{
		Cwd: "/home/dev/myapp", Bucket: budget.BucketWeek,
		Leases: []budget.Lease{{Kind: budget.LeaseManual}},
	}, time.Now())
	if err == nil {
		t.Fatal("stored a budget with neither rule set")
	}
	live, _ := s.ListBudgets(ctx, true)
	if len(live) != 0 {
		t.Errorf("a rejected budget left %d rows behind", len(live))
	}
}

func TestMetaRoundTrip(t *testing.T) {
	s, ctx := budgetStore(t)
	got, err := s.GetMeta(ctx, "announce_cursor")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("unset key returned %q, want empty", got)
	}
	if err := s.SetMeta(ctx, "announce_cursor", "412"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetMeta(ctx, "announce_cursor"); got != "412" {
		t.Errorf("got %q, want 412", got)
	}
	// Overwrite rather than conflict.
	if err := s.SetMeta(ctx, "announce_cursor", "500"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetMeta(ctx, "announce_cursor"); got != "500" {
		t.Errorf("got %q, want 500", got)
	}
}
