package ingest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// usageMap builds a rawUsage-shaped JSON object. cache_creation is only
// included when non-zero, matching what real transcripts do.
func usageMap(in, out, cr, cw5, cw1 int) map[string]any {
	u := map[string]any{
		"input_tokens":            in,
		"output_tokens":           out,
		"cache_read_input_tokens": cr,
	}
	if cw5 != 0 || cw1 != 0 {
		u["cache_creation"] = map[string]any{
			"ephemeral_5m_input_tokens": cw5,
			"ephemeral_1h_input_tokens": cw1,
		}
	}
	return u
}

// assistantRecord builds one type=="assistant" JSONL record. requestID and
// messageID are omitted from the JSON entirely when empty, so tests can
// exercise the "no requestId" and "neither" identity cases the fix has to
// handle without merging or dropping data.
func assistantRecord(ts, requestID, messageID, model string, usage map[string]any) map[string]any {
	rec := map[string]any{
		"type":      "assistant",
		"timestamp": ts,
	}
	if requestID != "" {
		rec["requestId"] = requestID
	}
	msg := map[string]any{
		"model": model,
		"usage": usage,
	}
	if messageID != "" {
		msg["id"] = messageID
	}
	rec["message"] = msg
	return rec
}

func writeJSONLFile(t *testing.T, name string, records []map[string]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	for _, r := range records {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal record: %v", err)
		}
		if _, err := f.Write(append(b, '\n')); err != nil {
			t.Fatalf("write record: %v", err)
		}
	}
	return path
}

// TestParseFile_DedupesRepeatedContentBlocks covers the core bug: three
// lines sharing (message.id, requestId) - as Claude Code writes for a
// text-plus-two-tool_use response - collapse into a single turn, and the
// surviving usage is the LAST line's (the one a streaming partial would
// finish on), not the first.
func TestParseFile_DedupesRepeatedContentBlocks(t *testing.T) {
	records := []map[string]any{
		assistantRecord("2026-01-01T00:00:00.000Z", "req_1", "msg_1", "claude-x", usageMap(10, 5, 0, 0, 0)),
		assistantRecord("2026-01-01T00:00:00.100Z", "req_1", "msg_1", "claude-x", usageMap(10, 15, 0, 0, 0)),
		assistantRecord("2026-01-01T00:00:00.200Z", "req_1", "msg_1", "claude-x", usageMap(10, 25, 0, 0, 0)),
	}
	path := writeJSONLFile(t, "session-abc12345.jsonl", records)

	fr, err := parseFile(path, nil)
	if err != nil {
		t.Fatalf("parseFile: %v", err)
	}
	if len(fr.Turns) != 1 {
		t.Fatalf("want 1 turn from 3 duplicate lines, got %d", len(fr.Turns))
	}
	got := fr.Turns[0]
	if got.OutputTokens != 25 {
		t.Errorf("want output_tokens from the LAST line (25), got %d", got.OutputTokens)
	}
	if got.TS != "2026-01-01T00:00:00.200Z" {
		t.Errorf("want timestamp from the last occurrence, got %q", got.TS)
	}
	if got.TurnIdx != 0 {
		t.Errorf("want turn_idx 0, got %d", got.TurnIdx)
	}
}

// TestParseFile_DedupesOnMessageIDWithoutRequestID covers the older
// transcript format that predates requestId: identity falls back to
// message.id alone.
func TestParseFile_DedupesOnMessageIDWithoutRequestID(t *testing.T) {
	records := []map[string]any{
		assistantRecord("2026-01-01T00:00:00.000Z", "", "msg_2", "claude-x", usageMap(1, 1, 0, 0, 0)),
		assistantRecord("2026-01-01T00:00:00.100Z", "", "msg_2", "claude-x", usageMap(1, 9, 0, 0, 0)),
	}
	path := writeJSONLFile(t, "session-abc12345.jsonl", records)

	fr, err := parseFile(path, nil)
	if err != nil {
		t.Fatalf("parseFile: %v", err)
	}
	if len(fr.Turns) != 1 {
		t.Fatalf("want 1 turn deduped on message.id alone, got %d", len(fr.Turns))
	}
	if fr.Turns[0].OutputTokens != 9 {
		t.Errorf("want last occurrence's output_tokens (9), got %d", fr.Turns[0].OutputTokens)
	}
}

// TestParseFile_NoDedupeWhenIdentityMissing covers records with neither
// message.id nor requestId: the fix must not drop or merge these just
// because it can't identify them, since that would be worse than the
// original bug.
func TestParseFile_NoDedupeWhenIdentityMissing(t *testing.T) {
	records := []map[string]any{
		assistantRecord("2026-01-01T00:00:00.000Z", "", "", "claude-x", usageMap(1, 1, 0, 0, 0)),
		assistantRecord("2026-01-01T00:00:00.100Z", "", "", "claude-x", usageMap(1, 2, 0, 0, 0)),
	}
	path := writeJSONLFile(t, "session-abc12345.jsonl", records)

	fr, err := parseFile(path, nil)
	if err != nil {
		t.Fatalf("parseFile: %v", err)
	}
	if len(fr.Turns) != 2 {
		t.Fatalf("want both unidentifiable records kept as unique turns, got %d", len(fr.Turns))
	}
	if fr.Turns[0].TurnIdx != 0 || fr.Turns[1].TurnIdx != 1 {
		t.Errorf("want dense turn_idx 0,1, got %d,%d", fr.Turns[0].TurnIdx, fr.Turns[1].TurnIdx)
	}
}

// TestParseFile_TurnIdxStaysDenseAcrossDuplicates makes sure a duplicate
// group sitting between two unique turns doesn't leave a hole in turn_idx
// (the primary key is (session_uuid, turn_idx), so it must stay 0,1,2,...).
func TestParseFile_TurnIdxStaysDenseAcrossDuplicates(t *testing.T) {
	records := []map[string]any{
		assistantRecord("2026-01-01T00:00:00Z", "req_A", "msg_A", "claude-x", usageMap(100, 1, 0, 0, 0)),
		// Duplicate group for response B: three lines, last one authoritative.
		assistantRecord("2026-01-01T00:16:40Z", "req_B", "msg_B", "claude-x", usageMap(100, 2, 0, 0, 0)),
		assistantRecord("2026-01-01T00:16:50Z", "req_B", "msg_B", "claude-x", usageMap(100, 3, 0, 0, 0)),
		assistantRecord("2026-01-01T00:18:20Z", "req_B", "msg_B", "claude-x", usageMap(100, 4, 0, 0, 0)),
		assistantRecord("2026-01-01T00:18:21Z", "req_C", "msg_C", "claude-x", usageMap(100, 5, 0, 0, 0)),
	}
	path := writeJSONLFile(t, "session-abc12345.jsonl", records)

	fr, err := parseFile(path, nil)
	if err != nil {
		t.Fatalf("parseFile: %v", err)
	}
	if len(fr.Turns) != 3 {
		t.Fatalf("want 3 turns (A, B-survivor, C), got %d", len(fr.Turns))
	}
	for i, turn := range fr.Turns {
		if turn.TurnIdx != i {
			t.Errorf("turn %d: want turn_idx %d, got %d (hole from a mishandled duplicate)", i, i, turn.TurnIdx)
		}
	}
	// gap_s for C must be measured against B's SURVIVING (last) timestamp
	// (00:18:20Z -> 00:18:21Z = 1s), not against an earlier duplicate line
	// in B's group, which a duplicate must never touch.
	if got := fr.Turns[2].GapS; got != 1 {
		t.Errorf("want gap_s(C) == 1s measured from B's last occurrence, got %v", got)
	}
}

// TestParseFile_CompactionConfirmUsesSurvivingPrefixOnly is the sharpest
// regression test for the ordering requirement: the assistant branch that
// follows a compaction summary both consumes pendingCompactReady (finalizing
// the compaction against the turn's prefix) AND updates prevPrefix/turnIdx.
// If a duplicate line reached that logic before being skipped, it would
// finalize the compaction against the WRONG (non-surviving) prefix and the
// real survivor would see no pending compaction left to confirm at all.
func TestParseFile_CompactionConfirmUsesSurvivingPrefixOnly(t *testing.T) {
	records := []map[string]any{
		// Baseline turn: prefix (input+cache) == 1000, establishes prevPrefix.
		assistantRecord("2026-01-01T00:00:00Z", "req_A", "msg_A", "claude-x", usageMap(1000, 1, 0, 0, 0)),
		{
			"type":      "system",
			"subtype":   "compact_boundary",
			"timestamp": "2026-01-01T00:01:00Z",
		},
		{
			"type":             "user",
			"isCompactSummary": true,
			"timestamp":        "2026-01-01T00:01:05Z",
			"message":          map[string]any{"content": "summary of compacted context"},
		},
		// Duplicate group for the post-compact turn. The first (would-be)
		// occurrence has a tiny prefix that WOULD confirm the compaction
		// (huge shrink) if it were wrongly processed as a real turn. Only
		// the LAST line - with a prefix too close to the baseline to count
		// as a real compaction - should ever reach the confirm logic.
		assistantRecord("2026-01-01T00:02:00Z", "req_B", "msg_B", "claude-x", usageMap(100, 1, 0, 0, 0)),
		assistantRecord("2026-01-01T00:02:01Z", "req_B", "msg_B", "claude-x", usageMap(950, 1, 0, 0, 0)),
	}
	path := writeJSONLFile(t, "session-abc12345.jsonl", records)

	fr, err := parseFile(path, nil)
	if err != nil {
		t.Fatalf("parseFile: %v", err)
	}
	if len(fr.Turns) != 2 {
		t.Fatalf("want 2 turns (A, B-survivor), got %d", len(fr.Turns))
	}
	if len(fr.Compactions) != 1 {
		t.Fatalf("want exactly 1 compaction record, got %d", len(fr.Compactions))
	}
	c := fr.Compactions[0]
	if c.Confirmed {
		t.Errorf("want confirmed=false: the surviving prefix (950) only shrinks 5%% from baseline (1000), " +
			"well under the 50%% threshold - a confirmed=true here means a duplicate leaked into the decision")
	}
	if c.ConfirmReason != "weak_shrink" {
		t.Errorf("want reason=weak_shrink, got %q", c.ConfirmReason)
	}
	if c.PrefixTokensEst != 1000 {
		t.Errorf("want baseline prefix 1000, got %d", c.PrefixTokensEst)
	}
	// The surviving post-compact turn must still be the one carrying
	// post_compact=true and the final (950) prefix - duplicates being
	// skipped must not have consumed the pending compaction early.
	b := fr.Turns[1]
	if !b.PostCompact {
		t.Errorf("want the surviving post-compact turn to have PostCompact=true")
	}
	if b.InputTokens != 950 {
		t.Errorf("want the surviving turn's input_tokens (950), got %d", b.InputTokens)
	}
}
