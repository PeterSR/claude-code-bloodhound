package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

// TestPrintHistoryRepairHuman_OrdersIngestBeforeRepair pins the one part
// of doctor's history-repair section that is a correctness claim rather
// than formatting: which command a user is told to run.
//
// `ingest --force` dedupes on the real message.id/requestId and so fixes
// any session whose JSONL still exists exactly; repair --dedupe-history
// is a 60s-window timing heuristic for sessions ingest can no longer
// reach. Recommending repair while transcripts are still un-ingested
// bakes an estimate into rows that could have been reconstructed
// precisely, and the resulting turn counts are not recoverable by
// re-running anything. The order is the whole point of the section.
func TestPrintHistoryRepairHuman_OrdersIngestBeforeRepair(t *testing.T) {
	st := store.RepairStats{
		SessionsInScope: 432,
		SessionsChanged: 377,
		RunsCollapsed:   26165,
		TurnsRemoved:    40386,
		RawTokensBefore: 20_000_000_000,
		RawTokensAfter:  11_720_000_000,
	}

	t.Run("transcripts still missing sends the user to ingest", func(t *testing.T) {
		var buf bytes.Buffer
		printHistoryRepairHuman(&buf, st, true)
		got := buf.String()

		if !strings.Contains(got, "ingest --force") {
			t.Errorf("want the ingest --force recommendation, got:\n%s", got)
		}
		if strings.Contains(got, "--apply") {
			t.Errorf("must not offer the destructive repair --apply while coverage is incomplete, got:\n%s", got)
		}
	})

	t.Run("coverage complete offers the repair, dry run first", func(t *testing.T) {
		var buf bytes.Buffer
		printHistoryRepairHuman(&buf, st, false)
		got := buf.String()

		if !strings.Contains(got, "repair --dedupe-history") {
			t.Errorf("want the repair recommendation, got:\n%s", got)
		}
		dry := strings.Index(got, "bloodhound repair --dedupe-history`")
		apply := strings.Index(got, "--dedupe-history --apply")
		if dry < 0 || apply < 0 || dry > apply {
			t.Errorf("want the dry run named before --apply, got:\n%s", got)
		}
	})

	t.Run("nothing to collapse stays quiet", func(t *testing.T) {
		var buf bytes.Buffer
		printHistoryRepairHuman(&buf, store.RepairStats{SessionsInScope: 632}, true)
		got := buf.String()

		if strings.Contains(got, "!!") || strings.Contains(got, "ingest --force") {
			t.Errorf("a clean database must not raise an alarm or prescribe anything, got:\n%s", got)
		}
	})
}
