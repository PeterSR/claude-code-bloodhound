package store

import (
	"context"
	"testing"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/usage"
)

// An api reading carries exact reset instants and says where it came from.
// The instant must be stored as is, not re-parsed from the raw string (which
// for the api is an ISO timestamp ParseReset was never meant to read).
func TestRecordUsage_APIReadingKeepsExactResetAndSource(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Add(-time.Minute)
	sessReset := at.Add(2 * time.Hour).Truncate(time.Minute)
	weekReset := at.Add(72 * time.Hour).Truncate(time.Minute)

	_, err := s.RecordUsage(ctx, 1, usage.Result{
		OK:              true,
		Source:          usage.SourceAPI,
		FetchedAt:       at,
		SessionPct:      pctOf(42),
		WeekPct:         pctOf(61),
		SessionResetRaw: sessReset.Format(time.RFC3339Nano),
		WeekResetRaw:    weekReset.Format(time.RFC3339Nano),
		SessionResetAt:  &sessReset,
		WeekResetAt:     &weekReset,
		Raw:             `{"five_hour":{}}`,
	}, nil)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	got, err := s.LatestUsage(ctx, 1)
	if err != nil || got == nil {
		t.Fatalf("latest: %v %v", got, err)
	}
	if got.Source != usage.SourceAPI {
		t.Errorf("source: %q", got.Source)
	}
	if got.SessionResetTSISO != sessReset.Format(time.RFC3339) || got.WeekResetTSISO != weekReset.Format(time.RFC3339) {
		t.Errorf("resets: %q %q", got.SessionResetTSISO, got.WeekResetTSISO)
	}
}

// Callers that predate the source field (and every row before migration 0017)
// read as pty.
func TestRecordUsage_UnsetSourceIsPTY(t *testing.T) {
	s := openTestStore(t)
	recordPct(t, s, time.Now().Add(-time.Minute), 10, 20)
	got, err := s.LatestUsage(context.Background(), 1)
	if err != nil || got == nil {
		t.Fatalf("latest: %v %v", got, err)
	}
	if got.Source != usage.SourcePTY {
		t.Errorf("source: %q", got.Source)
	}
}
