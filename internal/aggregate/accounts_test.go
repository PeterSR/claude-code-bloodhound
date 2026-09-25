package aggregate

import (
	"context"
	"testing"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/store"
	"github.com/PeterSR/claude-code-bloodhound/internal/usage"
)

// Two accounts spend in the same hour, and only account 1 has a meter. Its
// movement must be split among its own sessions alone: a turn on account 2
// never moved account 1's meter, and crediting it would both shrink account
// 1's real sessions and invent spend on a meter account 2 never touched.
func TestRun_AttributionStaysInsideEachAccount(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.DB.Exec(`INSERT INTO accounts (id, account_uuid) VALUES (2, 'acc-two')`); err != nil {
		t.Fatal(err)
	}

	base := time.Now().Add(-2 * time.Hour).Truncate(time.Minute)
	for i, pct := range []int{10, 12, 15, 20} {
		p, w := pct, pct/2
		reset := base.Add(4 * time.Hour).UTC().Format(time.RFC3339)
		res := usage.Result{
			OK: true, FetchedAt: base.Add(time.Duration(i) * 20 * time.Minute),
			SessionPct: &p, WeekPct: &w,
		}
		if _, err := s.RecordUsage(ctx, 1, res, nil); err != nil {
			t.Fatal(err)
		}
		// RecordUsage parses reset hints from the raw panel string; set the
		// parsed column directly so both readings share one window.
		if _, err := s.DB.Exec(`UPDATE usage_observations SET session_reset_ts = ? WHERE ts_unix_ms = ?`,
			reset, res.FetchedAt.UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}

	turn := func(session string, acct int64, i int) store.TurnRow {
		ts := base.Add(time.Duration(i)*20*time.Minute + 5*time.Minute)
		return store.TurnRow{
			SessionUUID: session, TurnIdx: i, TS: ts.UTC().Format(time.RFC3339), TSUnixMS: ts.UnixMilli(),
			Model: "claude-sonnet-4-5", InputTokens: 1000, OutputTokens: 1000,
			Classification: "normal", Project: "p", AccountID: acct,
		}
	}
	for _, sp := range []struct {
		session string
		acct    int64
	}{{"sess-one", 1}, {"sess-two", 2}} {
		var turns []store.TurnRow
		for i := 0; i < 3; i++ {
			turns = append(turns, turn(sp.session, sp.acct, i))
		}
		if err := s.ReplaceSessionData(ctx, store.SessionPersist{SessionUUID: sp.session, Project: "p", Turns: turns}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := Run(ctx, s, Options{}); err != nil {
		t.Fatal(err)
	}

	var leaked int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM session_attribution
		WHERE account_id = 1 AND session_uuid = 'sess-two'`).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Errorf("account 2's session has %d attribution rows on account 1's meter", leaked)
	}
	var ownPct float64
	if err := s.DB.QueryRow(`SELECT COALESCE(SUM(measured_pct), 0) FROM session_attribution
		WHERE account_id = 1 AND bucket = '5h' AND session_uuid = 'sess-one'`).Scan(&ownPct); err != nil {
		t.Fatal(err)
	}
	if ownPct <= 0 {
		t.Errorf("account 1's own session got %.2f measured pct, want all of the movement", ownPct)
	}
	var buckets2 int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM buckets WHERE account_id = 2`).Scan(&buckets2); err != nil {
		t.Fatal(err)
	}
	if buckets2 == 0 {
		t.Error("account 2's turns produced no token buckets of its own")
	}
}
