package store

import (
	"context"
	"testing"
)

// obsRow is a minimal usage_observations insert for the recompute test.
func insertObs(t *testing.T, s *Store, tsMS int64, sessPct, weekPct int) {
	t.Helper()
	_, err := s.DB.Exec(`
		INSERT INTO usage_observations (ts, ts_unix_ms, session_pct, week_pct, parse_ok)
		VALUES (?, ?, ?, ?, 1)`,
		"2026-01-01T00:00:00Z", tsMS, sessPct, weekPct)
	if err != nil {
		t.Fatalf("insert obs: %v", err)
	}
}

func flagsAt(t *testing.T, s *Store, tsMS int64) (sReset, wReset, sValid, wValid int) {
	t.Helper()
	err := s.DB.QueryRow(`
		SELECT session_reset_detected, week_reset_detected,
		       session_pct_valid, week_pct_valid
		FROM usage_observations WHERE ts_unix_ms = ?`, tsMS).
		Scan(&sReset, &wReset, &sValid, &wValid)
	if err != nil {
		t.Fatalf("read flags at %d: %v", tsMS, err)
	}
	return
}

// TestRecomputeUsageFlagsEndToEnd drives the whole backfill pass against the
// real migrated schema (migration 0008 columns included), covering both bugs
// the classifier exists to fix: a small reset the old 30-point threshold
// missed, and a recovering misparse.
func TestRecomputeUsageFlagsEndToEnd(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	ctx := context.Background()
	s, err := Open(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.DB.Close()

	// Session climbs 15->17->18 then resets to 0 (an 18-point drop the old
	// threshold ignored). Week holds at 34 with one misparsed 16 in the
	// middle (index by ts: 400 is the dip).
	const m = int64(5 * 60 * 1000)
	insertObs(t, s, 1*m, 15, 34)
	insertObs(t, s, 2*m, 17, 34)
	insertObs(t, s, 3*m, 18, 16) // week 16 = misparse
	insertObs(t, s, 4*m, 0, 34)  // session 0 = reset
	insertObs(t, s, 5*m, 1, 34)

	if err := s.RecomputeUsageFlags(ctx); err != nil {
		t.Fatalf("recompute: %v", err)
	}

	// Session reset detected exactly at the drop-to-zero.
	if sr, _, _, _ := flagsAt(t, s, 4*m); sr != 1 {
		t.Errorf("session reset at 4m = %d, want 1", sr)
	}
	if sr, _, _, _ := flagsAt(t, s, 3*m); sr != 0 {
		t.Errorf("session reset at 3m = %d, want 0 (no reset yet)", sr)
	}

	// Week misparse flagged invalid, and it is NOT a reset.
	if _, wr, _, wv := flagsAt(t, s, 3*m); wv != 0 || wr != 0 {
		t.Errorf("week at 3m: valid=%d reset=%d, want valid=0 reset=0 (misparse)", wv, wr)
	}
	// Its neighbours stay valid.
	if _, _, _, wv := flagsAt(t, s, 2*m); wv != 1 {
		t.Errorf("week at 2m valid=%d, want 1", wv)
	}
	if _, _, _, wv := flagsAt(t, s, 4*m); wv != 1 {
		t.Errorf("week at 4m valid=%d, want 1", wv)
	}

	// Idempotent: a second pass changes nothing and doesn't error.
	if err := s.RecomputeUsageFlags(ctx); err != nil {
		t.Fatalf("recompute (2nd): %v", err)
	}
	if sr, _, _, _ := flagsAt(t, s, 4*m); sr != 1 {
		t.Errorf("session reset after 2nd pass = %d, want 1", sr)
	}
}
