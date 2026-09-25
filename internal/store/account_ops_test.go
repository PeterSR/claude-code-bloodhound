package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/usage"
)

func identity(uuid string) AccountIdentity {
	return AccountIdentity{AccountUUID: uuid, OrgUUID: "org-" + uuid, Email: uuid + "@example.com"}
}

func TestObserveLogin_DefaultDirClaimsPlaceholderAndSwitchOpensInterval(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), ".claude")

	id, err := s.ObserveLogin(ctx, dir, true, identity("aaa"), 1_000)
	if err != nil {
		t.Fatal(err)
	}
	if id != PlaceholderAccountID {
		t.Fatalf("default dir's first account = %d, want the placeholder %d", id, PlaceholderAccountID)
	}

	// Seeing the same login again is not a switch.
	if _, err := s.ObserveLogin(ctx, dir, true, identity("aaa"), 2_000); err != nil {
		t.Fatal(err)
	}
	other, err := s.ObserveLogin(ctx, dir, true, identity("bbb"), 5_000)
	if err != nil {
		t.Fatal(err)
	}
	if other == id {
		t.Fatal("a different login reused the first account's row")
	}

	logins, err := s.LoginsFor(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(logins) != 2 || logins[0].FromMS != 0 || logins[1].FromMS != 5_000 {
		t.Fatalf("logins = %+v, want one from 0 and one from the switch at 5000", logins)
	}
	for _, c := range []struct {
		ts   int64
		want int64
	}{{-1, id}, {0, id}, {4_999, id}, {5_000, other}, {9_999_999, other}} {
		if got := ResolveAccount(logins, c.ts); got != c.want {
			t.Errorf("ResolveAccount(%d) = %d, want %d", c.ts, got, c.want)
		}
	}
}

func TestObserveLogin_PlaceholderMergesIntoAccountSeenElsewhereFirst(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := t.TempDir()

	// A pre-accounts reading, owned by the placeholder.
	pct := 40
	if _, err := s.RecordUsage(ctx, 0, usage.Result{OK: true, FetchedAt: time.UnixMilli(1_000), SessionPct: &pct}, nil); err != nil {
		t.Fatal(err)
	}

	work, err := s.ObserveLogin(ctx, filepath.Join(base, "work"), false, identity("aaa"), 2_000)
	if err != nil {
		t.Fatal(err)
	}
	if work == PlaceholderAccountID {
		t.Fatal("a non-default dir claimed the placeholder")
	}
	got, err := s.ObserveLogin(ctx, filepath.Join(base, ".claude"), true, identity("aaa"), 3_000)
	if err != nil {
		t.Fatal(err)
	}
	if got != work {
		t.Fatalf("default dir resolved to %d, want the existing account %d", got, work)
	}

	accts, err := s.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(accts) != 1 || accts[0].ID != work {
		t.Fatalf("accounts = %+v, want only %d after the merge", accts, work)
	}
	var owner int64
	if err := s.DB.QueryRow(`SELECT account_id FROM usage_observations`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != work {
		t.Fatalf("pre-accounts reading owned by %d, want %d", owner, work)
	}
}

// Two accounts polled in turn must not read as each other's resets. Before
// accounts, the flag pass saw one series, and A at 80% next to B at 10% is
// exactly the drop it calls a reset.
func TestRecordUsage_InterleavedAccountsAreSeparateSeries(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	a, b := int64(1), int64(2)
	if _, err := s.DB.Exec(`INSERT INTO accounts (id, account_uuid) VALUES (2, 'bbb')`); err != nil {
		t.Fatal(err)
	}
	ts := int64(1_700_000_000_000)
	for i := 0; i < 3; i++ {
		for _, r := range []struct {
			acct int64
			pct  int
		}{{a, 80 + i}, {b, 10 + i}} {
			pct := r.pct
			ts += 60_000
			if _, err := s.RecordUsage(ctx, r.acct, usage.Result{
				OK: true, FetchedAt: time.UnixMilli(ts), SessionPct: &pct, WeekPct: &pct,
			}, nil); err != nil {
				t.Fatal(err)
			}
		}
	}

	var resets, invalid int
	if err := s.DB.QueryRow(`
		SELECT SUM(session_reset_detected + week_reset_detected),
		       SUM((1 - session_pct_valid) + (1 - week_pct_valid))
		  FROM usage_observations`).Scan(&resets, &invalid); err != nil {
		t.Fatal(err)
	}
	if resets != 0 || invalid != 0 {
		t.Fatalf("resets=%d invalid=%d across two steadily rising accounts, want 0 and 0", resets, invalid)
	}
}
