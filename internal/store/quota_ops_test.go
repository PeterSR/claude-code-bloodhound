package store

import (
	"context"
	"testing"
	"time"
)

func saveSignals(t *testing.T, s *Store, ctx context.Context, session string, rows ...QuotaSignalRow) {
	t.Helper()
	if err := s.ReplaceSessionData(ctx, SessionPersist{
		SessionUUID:  session,
		Project:      "myapp",
		QuotaSignals: rows,
	}); err != nil {
		t.Fatalf("persist: %v", err)
	}
}

func refused(session string, at time.Time, reset time.Time, usingOverage bool) QuotaSignalRow {
	return QuotaSignalRow{
		SessionUUID:           session,
		TSUnixMS:              at.UnixMilli(),
		Kind:                  "rate_limited",
		Bucket:                "session",
		ResetTSUnixMS:         reset.UnixMilli(),
		OverageStatus:         "rejected",
		OverageDisabledReason: "out_of_credits",
		UsingOverage:          usingOverage,
		LowPriorityOffer:      "treatment",
	}
}

func priority(session, kind string, at time.Time) QuotaSignalRow {
	return QuotaSignalRow{SessionUUID: session, TSUnixMS: at.UnixMilli(), Kind: kind}
}

func TestARefusalIsEvidenceOnlyUntilItsWindowReopens(t *testing.T) {
	// The verdict answers "what is true now", so a refusal has to expire
	// with the window it was about. Without this the Now page would still be
	// reporting a closed window hours after it reopened, which is the same
	// failure as a stale percentage but harder to notice.
	s, ctx := budgetStore(t)
	now := time.Now()
	reset := now.Add(40 * time.Minute)
	saveSignals(t, s, ctx, "session-abc12345", refused("session-abc12345", now.Add(-time.Hour), reset, false))

	v, err := s.QuotaNow(ctx, 1, now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if !v.Refused || v.Bucket != "session" || !v.LowPriorityOffered {
		t.Fatalf("want a live refusal with the offer on it, got %+v", v)
	}

	after, err := s.QuotaNow(ctx, 1, reset.Add(time.Minute).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if after.Refused {
		t.Errorf("refusal outlived its own reset: %+v", after)
	}
}

func TestLowPriorityRunsUntilTheWindowItStandsInFor(t *testing.T) {
	s, ctx := budgetStore(t)
	now := time.Now()
	reset := now.Add(90 * time.Minute)
	saveSignals(t, s, ctx, "session-abc12345",
		refused("session-abc12345", now.Add(-5*time.Minute), reset, false),
		priority("session-abc12345", "low_priority_on", now.Add(-4*time.Minute)),
	)

	v, err := s.QuotaNow(ctx, 1, now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if !v.LowPriorityActive || !v.LowPriority("session-abc12345") {
		t.Fatalf("want the accepting session listed, got %+v", v)
	}
	if v.LowPriorityUntilMS != reset.UnixMilli() {
		t.Errorf("until = %d, want the refusal's own reset %d", v.LowPriorityUntilMS, reset.UnixMilli())
	}

	after, err := s.QuotaNow(ctx, 1, reset.Add(time.Minute).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if after.LowPriorityActive {
		t.Errorf("low priority outlived the window it was standing in for: %+v", after)
	}
}

func TestSwitchingLowPriorityOffEndsItEarly(t *testing.T) {
	s, ctx := budgetStore(t)
	now := time.Now()
	reset := now.Add(90 * time.Minute)
	saveSignals(t, s, ctx, "session-abc12345",
		refused("session-abc12345", now.Add(-30*time.Minute), reset, false),
		priority("session-abc12345", "low_priority_on", now.Add(-29*time.Minute)),
		priority("session-abc12345", "low_priority_off", now.Add(-2*time.Minute)),
	)

	v, err := s.QuotaNow(ctx, 1, now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if v.LowPriorityActive {
		t.Fatalf("newest signal was the switch-off: %+v", v)
	}
	if !v.Refused {
		t.Errorf("the window is still closed even though the mode was left: %+v", v)
	}
}

func TestOneSessionLeavingLowPriorityDoesNotSpeakForTheOthers(t *testing.T) {
	// The priority is per conversation. A shared flag would have the second
	// session told it had stopped working because the first one chose to
	// wait, which is the exact wrong thing to say to a session that is mid
	// turn and succeeding.
	s, ctx := budgetStore(t)
	now := time.Now()
	reset := now.Add(90 * time.Minute)
	saveSignals(t, s, ctx, "session-abc12345",
		refused("session-abc12345", now.Add(-30*time.Minute), reset, false),
		priority("session-abc12345", "low_priority_on", now.Add(-29*time.Minute)),
		priority("session-abc12345", "low_priority_off", now.Add(-2*time.Minute)),
	)
	saveSignals(t, s, ctx, "session-def67890",
		priority("session-def67890", "low_priority_on", now.Add(-20*time.Minute)),
	)

	v, err := s.QuotaNow(ctx, 1, now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if !v.LowPriority("session-def67890") {
		t.Errorf("second session lost its own priority: %+v", v)
	}
	if v.LowPriority("session-abc12345") {
		t.Errorf("first session kept a priority it switched off: %+v", v)
	}
}

func TestExtraUsageAndRefusalAreDifferentAnswers(t *testing.T) {
	// The distinction the whole table exists for.
	s, ctx := budgetStore(t)
	now := time.Now()
	saveSignals(t, s, ctx, "session-abc12345",
		refused("session-abc12345", now.Add(-time.Minute), now.Add(time.Hour), true))

	v, err := s.QuotaNow(ctx, 1, now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if !v.UsingOverage {
		t.Fatalf("want the overage flag preserved, got %+v", v)
	}
}

func TestNoSignalsMeansNoClaim(t *testing.T) {
	s, ctx := budgetStore(t)
	v, err := s.QuotaNow(ctx, 1, time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if v.Refused || v.LowPriorityActive || len(v.LowPrioritySessions) != 0 {
		t.Fatalf("want the zero verdict, got %+v", v)
	}
}

func TestReingestingASessionReplacesItsSignals(t *testing.T) {
	// Transcripts are re-parsed whole on every mtime change, the same as
	// turns are, so the rows have to be replaced rather than accumulated.
	s, ctx := budgetStore(t)
	now := time.Now()
	reset := now.Add(time.Hour)
	saveSignals(t, s, ctx, "session-abc12345",
		refused("session-abc12345", now.Add(-time.Minute), reset, false),
		priority("session-abc12345", "low_priority_on", now.Add(-30*time.Second)),
	)
	saveSignals(t, s, ctx, "session-abc12345",
		refused("session-abc12345", now.Add(-time.Minute), reset, false),
		priority("session-abc12345", "low_priority_on", now.Add(-30*time.Second)),
		priority("session-abc12345", "low_priority_off", now.Add(-10*time.Second)),
	)

	var n int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM quota_signals WHERE session_uuid = ?`, "session-abc12345",
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("row count = %d, want 3 (the second parse replacing the first)", n)
	}
}
