package ingest

import (
	"encoding/json"
	"strings"
)

// Quota signals are the things a transcript says about the account's quota
// verdict and about how this session's requests are being queued. They are
// the only place either fact is written down: the /usage panel bloodhound
// scrapes reports a percentage and a reset, and a percentage cannot tell
// "pinned at the cap and billing to extra usage" apart from "pinned at the
// cap and refused", which are opposite situations for the person working.
//
// Claude Code learned to offer a lower request priority instead of stopping
// when the five hour window closes (2.1.239 shipped the machinery; by
// 2.1.258 it is the offer on screen). A session that accepts keeps working
// off spare capacity against the weekly allowance. Read only the meter,
// and that session looks stopped at 100% with extra usage billing, which is
// wrong twice over.
const (
	// SignalRateLimited is a request the API refused for quota reasons. It
	// carries the authoritative reset moment and, more usefully, whether
	// the pay-per-use tier was actually available to absorb the spend.
	SignalRateLimited = "rate_limited"
	// SignalLowPriorityOn is this session accepting the lower priority.
	SignalLowPriorityOn = "low_priority_on"
	// SignalLowPriorityOff is it being switched back off by hand. Best
	// effort — see lowPriorityFromCommand.
	SignalLowPriorityOff = "low_priority_off"
)

// QuotaSignal is one such observation, keyed by the session that saw it.
// Account-level facts (the reset, whether overage is available) are read
// from whichever session happened to hit the wall; per-session facts (the
// priority a session is queued at) belong only to that session.
type QuotaSignal struct {
	SessionUUID string
	TSUnixMS    int64
	Kind        string

	// Bucket is which window the refusal was about, in the same vocabulary
	// the rest of bloodhound uses: "session" for the five hour window,
	// "week" for the seven day one, "" when the record did not say.
	Bucket string

	// ResetTSUnixMS is when the refused window reopens. Worth more than the
	// reset bloodhound parses out of the /usage panel: this one arrives as
	// a unix timestamp from the server rather than as "1:20pm" read off a
	// terminal and re-anchored to a local date.
	ResetTSUnixMS int64

	// OverageStatus is the pay-per-use tier's own verdict ("allowed",
	// "rejected"), and OverageDisabledReason says why when it refused
	// ("out_of_credits"). UsingOverage is whether spend was in fact landing
	// there at the moment of the refusal.
	OverageStatus         string
	OverageDisabledReason string
	UsingOverage          bool

	// LowPriorityOffer is non-empty when the low priority fallback was on
	// the table for this refusal. The value is the rollout arm Claude Code
	// reported ("treatment"), so it is stored rather than flattened to a
	// bool: a future arm name should not silently read as "not offered".
	LowPriorityOffer string

	Project string
	Cwd     string
}

// rawQuotaLimits is the server's quota verdict, attached to the 429 record
// Claude Code writes when a request is refused.
type rawQuotaLimits struct {
	Status                string `json:"status"`
	ResetsAt              int64  `json:"resetsAt"`
	RateLimitType         string `json:"rateLimitType"`
	OverageStatus         string `json:"overageStatus"`
	OverageDisabledReason string `json:"overageDisabledReason"`
	IsUsingOverage        bool   `json:"isUsingOverage"`
	LowPriorityOffer      string `json:"lowPriorityOffer"`
}

// quotaState is what reading one record needs to know about the ones before
// it. Both fields exist because the evidence for low priority is spread
// across records rather than contained in any one of them.
type quotaState struct {
	// lowPriToggle: a /low-priority invocation is awaiting its output. The
	// two halves of a slash command land as two records and which way the
	// toggle went is only legible from the second.
	lowPriToggle bool
	// resetMS: when the newest refusal seen so far in this file said the
	// window reopens. This is what tells an auto-continuation that was
	// served early apart from one that simply waited — see the comment on
	// the QueuePriority branch below.
	resetMS int64
}

// quotaSignals extracts whatever one record says about quota. Called for
// every line before the main switch rather than as a case inside it,
// because the record shapes overlap with ones that switch already claims:
// the refusal is a type "assistant" record and would otherwise have to be
// intercepted ahead of turn parsing, which is not this change's business.
func quotaSignals(rec rawRecord, sessionUUID, project string, st *quotaState) []QuotaSignal {
	tsMS := parseTSMS(rec.Timestamp)
	if tsMS == 0 {
		return nil
	}
	base := QuotaSignal{
		SessionUUID: sessionUUID,
		TSUnixMS:    tsMS,
		Project:     project,
		Cwd:         rec.Cwd,
	}

	if rec.QuotaLimits != nil && rec.Error == "rate_limit" {
		sig := base
		sig.Kind = SignalRateLimited
		sig.Bucket = bucketForRateLimitType(rec.QuotaLimits.RateLimitType)
		sig.ResetTSUnixMS = rec.QuotaLimits.ResetsAt * 1000
		sig.OverageStatus = rec.QuotaLimits.OverageStatus
		sig.OverageDisabledReason = rec.QuotaLimits.OverageDisabledReason
		sig.UsingOverage = rec.QuotaLimits.IsUsingOverage
		sig.LowPriorityOffer = rec.QuotaLimits.LowPriorityOffer
		if sig.ResetTSUnixMS > 0 {
			st.resetMS = sig.ResetTSUnixMS
		}
		return []QuotaSignal{sig}
	}

	// The prompt that came back through the queue and resumed the work the
	// refusal interrupted.
	//
	// All three conditions are load bearing, and the first two alone are not
	// enough. queuePriority is the general "this prompt was queued rather
	// than sent straight away" field: a scheduled wakeup, or anything typed
	// while a turn was running, carries "later" too, on builds that predate
	// low priority mode existing at all. Restricting it to an
	// auto-continuation narrows it to the resume-after-a-limit path, which
	// leaves one ambiguity — that path also fires when a session simply
	// waited out the window.
	//
	// The window's own reset separates them, and separates them exactly. An
	// auto-continuation served BEFORE the window reopens cannot have been
	// served by the window; the only thing that serves it is the lower
	// priority. One that fires at or after the reset is the ordinary wait
	// and means nothing.
	if rec.QueuePriority == "later" && rec.Origin != nil && rec.Origin.Kind == "auto-continuation" {
		if st.resetMS > 0 && tsMS < st.resetMS {
			sig := base
			sig.Kind = SignalLowPriorityOn
			return []QuotaSignal{sig}
		}
		return nil
	}

	if rec.Type == "system" && rec.Subtype == "local_command" {
		if kind, ok := lowPriorityFromCommand(rec.Content, &st.lowPriToggle); ok {
			sig := base
			sig.Kind = kind
			return []QuotaSignal{sig}
		}
		return nil
	}

	// The first half of the slash-command pair: the invocation itself,
	// which says a toggle happened without saying which way.
	if rec.Type == "user" && commandName(rec.Message) == "/low-priority" {
		st.lowPriToggle = true
	}
	return nil
}

// lowPriorityFromCommand reads the stdout half of a /low-priority
// invocation. This is the primary signal, not the fallback: it is written
// at the moment the mode is switched on, it says so in as many words, and
// it needs no corroboration from the records around it.
//
// Asymmetric on purpose. Switching it on prints a sentence bloodhound can
// recognise on its own ("Continuing now at lower priority until your limit
// resets at ..."), so that is matched directly and does not depend on
// having seen the invocation. Switching it back off prints something this
// code has never seen, so the only thing it can do is notice that a
// /low-priority invocation produced output that was not the on-sentence.
//
// Getting the off case wrong is the cheap direction. Low priority ends at
// the window reset regardless, and every reader of these signals bounds it
// by that reset, so an unrecognised switch-off shows as low priority for
// the remainder of a window that was already closed.
func lowPriorityFromCommand(content json.RawMessage, lowPriToggle *bool) (string, bool) {
	var text string
	if err := json.Unmarshal(content, &text); err != nil {
		return "", false
	}
	if strings.Contains(text, "at lower priority") && strings.Contains(text, "Continuing") {
		*lowPriToggle = false
		return SignalLowPriorityOn, true
	}
	if *lowPriToggle {
		*lowPriToggle = false
		return SignalLowPriorityOff, true
	}
	return "", false
}

// commandName pulls the slash command out of a <command-name> wrapper,
// normalised to a leading slash. Returns "" for anything else, including
// the tag being absent, which is the common case.
func commandName(msg json.RawMessage) string {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(msg, &m); err != nil {
		return ""
	}
	var text string
	if err := json.Unmarshal(m.Content, &text); err != nil {
		return ""
	}
	const open, close = "<command-name>", "</command-name>"
	i := strings.Index(text, open)
	if i < 0 {
		return ""
	}
	rest := text[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		return ""
	}
	name := strings.TrimSpace(rest[:j])
	if name != "" && !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	return name
}

// bucketForRateLimitType maps the server's name for a window onto the one
// the events log, the budget package and the Now page already use. An
// unrecognised type returns "" rather than guessing: a signal that cannot
// say which window it is about is still worth keeping for its overage
// verdict, which is not per-window.
func bucketForRateLimitType(t string) string {
	switch t {
	case "five_hour":
		return "session"
	case "seven_day", "weekly", "week":
		return "week"
	}
	return ""
}
