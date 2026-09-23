package ingest

import "testing"

// The refusal record, shaped the way Claude Code 2.1.258 writes it: a
// synthetic assistant message carrying the server's quotaLimits verdict.
func refusalRecord(ts string, resetsAt int64, overageStatus, reason, offer string) map[string]any {
	return map[string]any{
		"type":              "assistant",
		"timestamp":         ts,
		"error":             "rate_limit",
		"isApiErrorMessage": true,
		"apiErrorStatus":    429,
		"quotaLimits": map[string]any{
			"status":                       "rejected",
			"resetsAt":                     resetsAt,
			"rateLimitType":                "five_hour",
			"overageStatus":                overageStatus,
			"overageDisabledReason":        reason,
			"isUsingOverage":               false,
			"lowPriorityOffer":             offer,
			"lowPriorityRetryAfterSeconds": 20,
			"lowPriorityMaxWaitSeconds":    1200,
		},
		"message": map[string]any{
			"model": "<synthetic>",
			"role":  "assistant",
			"content": []any{map[string]any{
				"type": "text",
				"text": "You've hit your session limit",
			}},
			"usage": usageMap(0, 0, 0, 0, 0),
		},
	}
}

func systemRecord(ts, subtype, content string) map[string]any {
	return map[string]any{
		"type":      "system",
		"timestamp": ts,
		"subtype":   subtype,
		"content":   content,
	}
}

func commandRecord(ts, name string) map[string]any {
	return map[string]any{
		"type":      "user",
		"timestamp": ts,
		"message": map[string]any{
			"role":    "user",
			"content": "<command-name>" + name + "</command-name>\n<command-args></command-args>",
		},
	}
}

func signalKinds(sigs []QuotaSignal) []string {
	out := make([]string, 0, len(sigs))
	for _, s := range sigs {
		out = append(out, s.Kind)
	}
	return out
}

func TestRefusalCarriesTheServersOwnVerdict(t *testing.T) {
	// The whole reason to read this record rather than the /usage panel: it
	// says whether the pay-per-use tier absorbed the overflow. A percentage
	// pinned at 100 says nothing about that, and the gauge used to assert
	// extra usage was billing on exactly this evidence.
	path := writeJSONLFile(t, "session-abc12345.jsonl", []map[string]any{
		refusalRecord("2026-09-02T10:19:11.876Z", 1788348000, "rejected", "out_of_credits", "treatment"),
	})
	fr, err := parseFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(fr.QuotaSignals) != 1 {
		t.Fatalf("want 1 signal, got %v", signalKinds(fr.QuotaSignals))
	}
	q := fr.QuotaSignals[0]
	if q.Kind != SignalRateLimited {
		t.Errorf("kind = %q, want %q", q.Kind, SignalRateLimited)
	}
	if q.Bucket != "session" {
		t.Errorf("bucket = %q, want session (from five_hour)", q.Bucket)
	}
	if q.ResetTSUnixMS != 1788348000*1000 {
		t.Errorf("reset = %d, want the resetsAt in milliseconds", q.ResetTSUnixMS)
	}
	if q.OverageStatus != "rejected" || q.OverageDisabledReason != "out_of_credits" || q.UsingOverage {
		t.Errorf("overage verdict lost: %+v", q)
	}
	if q.LowPriorityOffer != "treatment" {
		t.Errorf("offer = %q, want treatment", q.LowPriorityOffer)
	}
}

func TestRefusalStillCountsAsATurn(t *testing.T) {
	// The signal is read ahead of the switch rather than as a case in it, so
	// that turn parsing sees exactly what it saw before. This asserts the
	// "ahead of" part: a change that intercepted the record would silently
	// drop a row the turns table has always had.
	path := writeJSONLFile(t, "session-abc12345.jsonl", []map[string]any{
		refusalRecord("2026-09-02T10:19:11.876Z", 1788348000, "rejected", "out_of_credits", "treatment"),
	})
	fr, err := parseFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(fr.Turns) != 1 {
		t.Fatalf("want the refusal still parsed as a turn, got %d", len(fr.Turns))
	}
}

// continuation builds the prompt Claude Code re-injects to pick the work back
// up after a limit. resumedAt decides everything: before the reset it can only
// have been served at the lower priority, at or after it, it is the ordinary
// wait.
func continuation(ts string) map[string]any {
	return map[string]any{
		"type":          "user",
		"timestamp":     ts,
		"queuePriority": "later",
		"origin":        map[string]any{"kind": "auto-continuation"},
		"promptSource":  "system",
		"isMeta":        true,
		"message":       map[string]any{"role": "user", "content": "You can continue now."},
	}
}

func TestAContinuationServedBeforeTheResetCorroboratesLowPriority(t *testing.T) {
	// Two records say low priority started: the slash command's stdout, and
	// a continuation that came back before the window it was waiting on had
	// reopened. Both are read.
	path := writeJSONLFile(t, "session-abc12345.jsonl", []map[string]any{
		refusalRecord("2026-09-02T10:19:11.876Z", 1788348000, "rejected", "out_of_credits", "treatment"),
		commandRecord("2026-09-02T10:20:09.000Z", "/low-priority"),
		systemRecord("2026-09-02T10:20:09.551Z", "local_command",
			"<local-command-stdout>Continuing now at lower priority until your limit resets at 1:20pm.</local-command-stdout>"),
		continuation("2026-09-02T10:20:09.584Z"),
	})
	fr, err := parseFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := signalKinds(fr.QuotaSignals)
	want := []string{SignalRateLimited, SignalLowPriorityOn, SignalLowPriorityOn}
	if len(got) != len(want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", got, want)
		}
	}
}

func TestSwitchingLowPriorityBackOffIsRecognisedByItsInvocation(t *testing.T) {
	// The off sentence is one this code has never seen. What it can see is a
	// /low-priority invocation whose output was not the on sentence, which is
	// enough to stop claiming the session is still working.
	path := writeJSONLFile(t, "session-abc12345.jsonl", []map[string]any{
		systemRecord("2026-09-02T10:20:09.551Z", "local_command",
			"<local-command-stdout>Continuing now at lower priority until your limit resets at 1:20pm.</local-command-stdout>"),
		commandRecord("2026-09-02T10:40:00.000Z", "/low-priority"),
		systemRecord("2026-09-02T10:40:00.100Z", "local_command",
			"<local-command-stdout>Waiting for your limit to reset at 1:20pm.</local-command-stdout>"),
	})
	fr, err := parseFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := signalKinds(fr.QuotaSignals)
	if len(got) != 2 || got[0] != SignalLowPriorityOn || got[1] != SignalLowPriorityOff {
		t.Fatalf("kinds = %v, want [%s %s]", got, SignalLowPriorityOn, SignalLowPriorityOff)
	}
}

func TestAQueuedPromptIsNotALowPrioritySignal(t *testing.T) {
	// The trap this guards. queuePriority is the general "queued rather than
	// sent straight away" field, and a scheduled prompt or anything typed
	// while a turn was running carries "later" too — including on builds
	// from before low priority mode existed. Read alone it puts sessions
	// into a mode they were never in, on transcripts months old.
	path := writeJSONLFile(t, "session-abc12345.jsonl", []map[string]any{
		{
			"type":          "user",
			"timestamp":     "2026-08-11T19:30:42.772Z",
			"queuePriority": "later",
			"promptSource":  "system",
			"isMeta":        true,
			"message":       map[string]any{"role": "user", "content": "Safety-net tick."},
		},
	})
	fr, err := parseFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(fr.QuotaSignals) != 0 {
		t.Fatalf("want no signals, got %v", signalKinds(fr.QuotaSignals))
	}
}

func TestAContinuationAtTheResetIsJustTheWindowReopening(t *testing.T) {
	// The other half of the same discrimination. A session that waited the
	// window out resumes through the same code path and the same fields; the
	// only thing that differs is that it resumed once the window was open.
	path := writeJSONLFile(t, "session-abc12345.jsonl", []map[string]any{
		refusalRecord("2026-09-02T10:19:11.876Z", 1788348000, "rejected", "out_of_credits", "treatment"),
		// 1788348000 is 11:20:00Z, so this is a minute past the reopening.
		continuation("2026-09-02T11:21:00.000Z"),
	})
	fr, err := parseFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := signalKinds(fr.QuotaSignals)
	if len(got) != 1 || got[0] != SignalRateLimited {
		t.Fatalf("kinds = %v, want only the refusal", got)
	}
}

func TestUnrelatedSlashCommandOutputSaysNothingAboutPriority(t *testing.T) {
	// Every session is full of local_command records. Only the ones that
	// followed a /low-priority invocation are allowed to mean anything, or
	// the first /status of the day would switch the mode off.
	path := writeJSONLFile(t, "session-abc12345.jsonl", []map[string]any{
		commandRecord("2026-09-02T10:30:33.000Z", "/status"),
		systemRecord("2026-09-02T10:30:33.874Z", "local_command",
			"<local-command-stdout>Settings dialog dismissed</local-command-stdout>"),
	})
	fr, err := parseFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(fr.QuotaSignals) != 0 {
		t.Fatalf("want no signals, got %v", signalKinds(fr.QuotaSignals))
	}
}
