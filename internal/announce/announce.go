// Package announce carries pressure from the event log into the conversations
// that need to hear about it.
//
// This is the delivery half of what claude-usage-governor did with its plugin
// monitor. That channel was narrow in three ways this one is not: lines were
// clipped near 512 characters, a monitor emitting too much was stopped by the
// host with no documented threshold, and it had to be armed at session start
// so it could never reach a session already running. Delivery here goes over
// the session inbox socket instead, which has none of those limits.
//
// Removing the limits makes restraint a design problem rather than a channel
// property, and the restraint is the point. Two rules do the work.
//
// Only transitions are announced. A level that has not changed is not news,
// and the event log already records exactly the transitions, so there is
// nothing to diff here: if it is in the log it just became true.
//
// Only awake sessions are told. That rule lives in notify.Gate, which explains
// itself at length; the short version is that delivering to a session sitting
// at its prompt starts a turn it would not otherwise take, and if its cache
// has gone cold that turn re-pays the whole conversation prefix before reading
// a word. Missing a warning is cheap. A cold wakeup is not, and it is charged
// to the budget the warning was about.
package announce

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	ccsock "github.com/PeterSR/claude-code-socket-transport"

	"github.com/PeterSR/claude-code-bloodhound/internal/budget"
	"github.com/PeterSR/claude-code-bloodhound/internal/events"
	"github.com/PeterSR/claude-code-bloodhound/internal/notify"
	"github.com/PeterSR/claude-code-bloodhound/internal/projectconfig"
	"github.com/PeterSR/claude-code-bloodhound/internal/sessioninsight"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
	"github.com/PeterSR/claude-code-bloodhound/internal/whenfmt"
)

// cursorKey remembers the last event announced, so a restart does not replay
// the log into every conversation on the machine.
const cursorKey = "announce_cursor"

// maxPerPass bounds how many events one pass will speak about. A burst of
// transitions usually means several buckets crossed at once and the reader
// needs the worst of them, not all of them; the rest stay in the log where
// `bloodhound events` can show them.
const maxPerPass = 3

// backfillLimit bounds what a first run, or a run after a long gap, will look
// at. Without it a machine that has been collecting for months would announce
// its entire history the first time this is switched on.
const backfillLimit = 25

// Announceable kinds, and what each one means to a reader.
//
// Deliberately narrow. The event log carries collection health, thresholds at
// every ten percent, and saturation, and most of that is for a dashboard
// rather than an interruption. What earns a line in someone's conversation is
// pressure they can still act on.
var announceable = map[string]bool{
	// A directory's own allowance, the thing a user explicitly asked to be
	// warned about.
	"budget": true,
	// The 5h and weekly cliffs, on whether or not a budget exists. These are
	// the "always on" pressure: reaching 100% stops work mid-flight and has to
	// be resumed by hand, so it is a cliff rather than a slope.
	"limit_projection": true,
	"saturation":       true,
}

// Stats is what one pass did, for the daemon log.
type Stats struct {
	Considered int
	Announced  int
	Delivered  int
	Skipped    map[string]int
	Errors     []string
}

// Options configure a pass. The zero value is the normal one.
type Options struct {
	Now time.Time
	// DryRun resolves and gates everything but sends nothing, and does not
	// advance the cursor. What `bloodhound budget announce --dry-run` uses.
	DryRun bool
	// Gate overrides the default gate. Tests set this; production does not.
	Gate *notify.Gate
	// Sender overrides the transport. Tests set this; production does not.
	Sender Sender
}

// Sender is the transport seam, so a test can observe delivery without a live
// Claude Code session.
type Sender interface {
	Send(ctx context.Context, t notify.Target, text string) (string, error)
}

// Run reads the events recorded since the last pass and tells whoever is awake
// and concerned.
func Run(ctx context.Context, s *store.Store, opt Options) (Stats, error) {
	st := Stats{Skipped: map[string]int{}}
	now := opt.Now
	if now.IsZero() {
		now = time.Now()
	}

	cursor, set, err := readCursor(ctx, s)
	if err != nil {
		return st, err
	}

	head, err := events.MaxID(ctx, s.DB)
	if err != nil {
		return st, err
	}
	if !set {
		// First run. Start at the head rather than the beginning: nobody wants
		// their first switch-on to be a recital of everything that has ever
		// happened, and the levels are all still readable in the log.
		//
		// Keyed on whether the cursor exists, not on whether it is zero. A
		// first run against an empty log legitimately records cursor 0, and
		// treating that as "never initialised" would make every subsequent
		// pass re-initialise and announce nothing, forever.
		if !opt.DryRun {
			return st, writeCursor(ctx, s, head)
		}
		cursor = maxInt64(0, head-backfillLimit)
	}
	if head <= cursor {
		return st, nil
	}

	kinds := make([]string, 0, len(announceable))
	for k := range announceable {
		kinds = append(kinds, k+".*")
	}
	sort.Strings(kinds)

	// Look at the NEWEST window of the backlog, not the oldest.
	//
	// Query pages ascending, so asking for `backfillLimit` rows straight from
	// the cursor returns the oldest ones and then the cursor jumps to head,
	// which silently discards everything newer. After a real gap (daemon down
	// while a cron reconcile kept appending) that is exactly backwards: it
	// delivers stale transitions, possibly an "exceeded" that has since
	// cleared, and drops the ones still true.
	from := cursor
	if head-cursor > backfillLimit {
		from = head - backfillLimit
		log := head - cursor - backfillLimit
		st.Skipped[fmt.Sprintf("older than the last %d events", backfillLimit)] += int(log)
	}

	evs, err := events.Query(ctx, s.DB, events.Filter{
		SinceID: from,
		Kinds:   kinds,
		Limit:   backfillLimit,
	})
	if err != nil {
		return st, err
	}
	// Query has no upper bound, so a concurrent writer can land an event
	// between MaxID and here. Announcing it while writing the cursor at head
	// would announce it again next pass, so it waits for the pass that owns it.
	evs = upTo(evs, head)
	st.Considered = len(evs)

	// Claim the cursor BEFORE delivering rather than after.
	//
	// Two processes can run a pass at once (the daemon tick and a hand-run
	// `budget announce`), and delivery is slow enough that both would
	// otherwise read the same cursor and say the same thing to the same
	// session. Claiming first makes it at-most-once instead of at-most-twice,
	// at the cost of losing a batch if delivery dies mid-flight. That trade is
	// the same asymmetry the gate is built on: a warning nobody hears is
	// cheap, and the duplicate is not.
	if !opt.DryRun {
		if err := writeCursor(ctx, s, head); err != nil {
			return st, err
		}
	}

	worth := filterWorthSaying(evs)
	if len(worth) > maxPerPass {
		st.Skipped["not the most recent transition"] += len(worth) - maxPerPass
		worth = worth[len(worth)-maxPerPass:]
	}
	st.Announced = len(worth)

	if len(worth) > 0 {
		if err := deliver(ctx, s, now, worth, opt, &st); err != nil {
			st.Errors = append(st.Errors, err.Error())
		}
	}
	return st, nil
}

// upTo drops events past the head this pass claimed.
func upTo(evs []events.Event, head int64) []events.Event {
	out := evs[:0:0]
	for _, e := range evs {
		if e.ID <= head {
			out = append(out, e)
		}
	}
	return out
}

// filterWorthSaying drops the transitions that are not news to a reader.
//
// The log records every edge in both directions, which is right for a log and
// wrong for an interruption: nobody needs telling that pressure went away. The
// exception is a budget returning to clear, which is dropped too, because the
// reader either already heard the warning or was asleep for it.
func filterWorthSaying(evs []events.Event) []events.Event {
	out := evs[:0:0]
	for _, e := range evs {
		kind, state, ok := splitKind(e.Kind)
		if !ok || !announceable[kind] {
			continue
		}
		switch kind {
		case "budget":
			if state == budget.StateTight || state == budget.StateExceeded {
				out = append(out, e)
			}
		case "limit_projection":
			if state == "projected" {
				out = append(out, e)
			}
		case "saturation":
			if state == "saturated" {
				out = append(out, e)
			}
		}
	}
	return out
}

func splitKind(k string) (kind, state string, ok bool) {
	i := strings.LastIndex(k, ".")
	if i <= 0 || i == len(k)-1 {
		return "", "", false
	}
	return k[:i], k[i+1:], true
}

func deliver(ctx context.Context, s *store.Store, now time.Time, evs []events.Event, opt Options, st *Stats) error {
	sessions, err := ccsock.ListSessions()
	if err != nil {
		return fmt.Errorf("read session registry: %w", err)
	}
	if len(sessions) == 0 {
		return nil
	}

	gate := notify.Gate{Now: now}
	if opt.Gate != nil {
		gate = *opt.Gate
	}
	if gate.Cache == nil {
		gate.Cache = coldCache(ctx, s, sessions, now)
	}

	admitted, skipped := gate.Admitted(sessions)
	for k, v := range skipped {
		st.Skipped[k] += v
	}
	if len(admitted) == 0 {
		return nil
	}

	sender := opt.Sender
	if sender == nil {
		sender = notify.New()
	}

	for _, sess := range admitted {
		text := textFor(evs, sess, now, wakeupNudgeWanted(sess.CWD))
		if text == "" {
			continue // nothing in this batch concerns this session
		}
		if opt.DryRun {
			st.Delivered++
			continue
		}
		if _, err := sender.Send(ctx, notify.Target{SessionID: sess.SessionID}, text); err != nil {
			if notify.Undeliverable(err) {
				st.Skipped[notify.SkipUnreachable]++
				continue
			}
			st.Errors = append(st.Errors, fmt.Sprintf("%s: %v", sess.SessionID, err))
			continue
		}
		st.Delivered++
	}
	return nil
}

// textFor builds the line one session should hear, or "" when none of the
// batch concerns it.
//
// A budget event is directory-scoped, so it goes only to sessions running in
// that directory: telling an unrelated project that someone else's allowance
// is tight is noise, and it is the mistake a global broadcast makes. The
// limit events are account-wide and go to everyone admitted, because the 5h
// cliff stops every session on the machine, not just the one that caused it.
func textFor(evs []events.Event, sess ccsock.Session, now time.Time, nudgeWakeup bool) string {
	var lines []string
	// The soonest reset among the lines that made it in, and which bucket it
	// belongs to. Two buckets crossing together is one situation with two
	// deadlines, and the note has to name which of them it means.
	var soonest time.Time
	soonestBucket := ""
	for _, e := range evs {
		if e.Scope.Cwd != "" && !sameDir(e.Scope.Cwd, sess.CWD) {
			continue
		}
		line := describe(e, now)
		if line == "" {
			continue
		}
		lines = append(lines, line)
		if at, ok := resetAt(e.Detail["reset_ts"], now); ok && (soonest.IsZero() || at.Before(soonest)) {
			soonest, soonestBucket = at, e.Scope.Bucket
		}
	}
	if len(lines) == 0 {
		return ""
	}
	// Once for the batch, not once per line: several buckets crossing at the
	// same moment is one situation, and the same suggestion repeated three
	// times reads as a tool that has stopped paying attention.
	//
	// Only when a reset actually made it into the text above. Without one the
	// note would point at a moment the reader was never told, and there would
	// be nothing to arm a wakeup for.
	if nudgeWakeup && soonestBucket != "" {
		lines = append(lines, wakeupNote(soonestBucket))
	}
	return strings.Join(lines, "\n")
}

// wakeupNote is the opt-in tail on a warning, enabled per directory by
// projectconfig.WakeupNudge.
//
// Offered rather than ordered, and it names no mechanism. Bloodhound does not
// arm anything and has no idea what the reader schedules wakeups with; what it
// knows is that the work is about to stop and when it could start again, which
// is the part worth saying out loud.
//
// It names the bucket rather than repeating the time, which is already in the
// line above it, and rather than saying "that reset", which is ambiguous on
// the batch where both windows crossed at once.
func wakeupNote(bucket string) string {
	return fmt.Sprintf(
		"Work that stops here could pick up again when the %s window reopens, if something is armed to wake it.",
		budget.BucketLabel(bucket))
}

// wakeupNudgeWanted asks the directory a session is working in whether it
// wants the note.
//
// Silent on every failure. A directory with no file, an unreadable one, or a
// session with no cwd at all all mean the same thing here: nobody asked for
// the extra line, so it is not added.
func wakeupNudgeWanted(cwd string) bool {
	if cwd == "" {
		return false
	}
	cfg, _, err := projectconfig.Load(cwd)
	if err != nil {
		return false
	}
	return cfg.WakeupNudge
}

func sameDir(a, b string) bool {
	na, err1 := budget.NormalizeCwd(a)
	nb, err2 := budget.NormalizeCwd(b)
	if err1 != nil || err2 != nil {
		return a == b
	}
	return na == nb
}

// describe renders one event as a sentence a reader can act on.
//
// Phrased as an observation, never an instruction. Bloodhound measures; what
// to do about the measurement is the reader's call, and a monitoring tool that
// starts issuing orders into conversations is one users turn off.
func describe(e events.Event, now time.Time) string {
	kind, state, ok := splitKind(e.Kind)
	if !ok {
		return ""
	}
	b := budget.BucketLabel(e.Scope.Bucket)
	reset := when(e.Detail["reset_ts"], now)

	switch kind {
	case "budget":
		line := fmt.Sprintf("Budget for %s is %s.", budget.Label(e.Scope.Cwd), state)
		if reason, _ := e.Detail["reason"].(string); reason != "" {
			line = "Budget: " + reason + "."
		}
		if reset != "" {
			line += fmt.Sprintf(" The %s window resets %s.", b, reset)
		}
		return line
	case "limit_projection":
		eta := when(e.Detail["eta_ts"], now)
		switch {
		case eta != "" && reset != "":
			return fmt.Sprintf("The %s meter is on pace to reach 100%% %s, before it resets %s.", b, eta, reset)
		case eta != "":
			return fmt.Sprintf("The %s meter is on pace to reach 100%% %s, before it resets.", b, eta)
		case reset != "":
			return fmt.Sprintf("The %s meter is on pace to reach 100%% before it resets %s.", b, reset)
		}
		return fmt.Sprintf("The %s meter is on pace to reach 100%% before it resets.", b)
	case "saturation":
		line := fmt.Sprintf("The %s meter has stopped moving at its cap, so every figure downstream is now an estimate.", b)
		if reset != "" {
			line += fmt.Sprintf(" It resets %s.", reset)
		}
		return line
	}
	return ""
}

// when renders a stored RFC 3339 moment as a sentence fragment on the
// reader's clock: "in 42m", "on fri 15:30".
//
// Empty for anything it cannot stand behind, and every caller above is
// written to read as a sentence without it. A moment that has already passed
// is one of those cases: events are read after they are written, and a
// window that turned over between the reading and the delivery would
// otherwise be announced as resetting in the past.
func when(v any, now time.Time) string {
	at, ok := resetAt(v, now)
	if !ok {
		return ""
	}
	return whenfmt.Phrase(at, now)
}

// resetAt parses a stored moment and reports whether it is still ahead. The
// two callers want different halves of the same answer: one renders it, the
// other compares it against the other buckets'.
func resetAt(v any, now time.Time) (time.Time, bool) {
	iso, _ := v.(string)
	if iso == "" {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, iso)
	if err != nil || !at.After(now) {
		return time.Time{}, false
	}
	return at, true
}

// coldCache asks bloodhound's own transcripts which sessions would have to
// re-pay their prefix. Best effort: a session it has never ingested is simply
// absent, which the gate reads as unknown rather than warm.
func coldCache(ctx context.Context, s *store.Store, sessions []ccsock.Session, now time.Time) map[string]notify.CacheState {
	tokensPerPctCW, _, _, hasCal, _ := s.LatestCalibrationMedian(ctx, "session", 10)

	out := make(map[string]notify.CacheState, len(sessions))
	for _, sess := range sessions {
		if sess.SessionID == "" {
			continue
		}
		ref, err := sessioninsight.BySessionUUID(ctx, s.DB, sess.SessionID)
		if err != nil || ref == nil {
			continue // never ingested; the gate reads absence as unknown
		}
		in := sessioninsight.ForSession(ctx, s.DB, *ref, now, tokensPerPctCW, hasCal)
		if in == nil {
			continue
		}
		// ColdResumeCostCWTokens is only meaningful when the TTL was inferable.
		// With CacheTTLS == 0 the cost is left at 0 because nothing could be
		// priced, not because the cache is warm, and writing an entry here
		// would hand the gate a confident "warm" for exactly the sessions
		// bloodhound understands least. Absent means unknown; keep it absent.
		if in.CacheTTLS == 0 {
			continue
		}
		out[sess.SessionID] = notify.CacheState{
			Cold:                   in.ColdResumeCostCWTokens > 0,
			ColdResumeCostCWTokens: in.ColdResumeCostCWTokens,
		}
	}
	return out
}

// readCursor returns the recorded cursor and whether one has ever been
// written. The two are separate answers: an absent cursor means this install
// has never run a pass, while a cursor of 0 means it has and the log was empty
// at the time.
func readCursor(ctx context.Context, s *store.Store) (int64, bool, error) {
	v, err := s.GetMeta(ctx, cursorKey)
	if err != nil {
		return 0, false, err
	}
	if v == "" {
		return 0, false, nil
	}
	var id int64
	if _, err := fmt.Sscanf(v, "%d", &id); err != nil {
		// Unreadable is not the same as unset: re-initialise from head rather
		// than replaying the whole log.
		return 0, false, nil
	}
	return id, true, nil
}

func writeCursor(ctx context.Context, s *store.Store, id int64) error {
	return s.SetMeta(ctx, cursorKey, fmt.Sprintf("%d", id))
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
