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
//
// # The two exceptions, and why they are the same rule
//
// Both are opt-in per directory, and neither is a softening of the rule above.
// They are the two cases where the arithmetic that makes the rule right points
// the other way.
//
// A warm cache inside its last stretch. Delivering there starts a turn, but it
// starts it at warm-cache rates, and the alternative is that the same context
// is rebuilt from nothing the next time anyone touches the session. Speaking
// while the cache is warm is the cheap branch, not the expensive one, which is
// why cache.expiring is deliverable to a resting session while cache.expired
// never is.
//
// A promised wakeup. When a project asks bloodhound to carry the wakeup rather
// than suggest one, the cold resume on the far side is not an accident, it is
// the thing being paid for. Refusing it because it is expensive would mean the
// one delivery a user explicitly asked for is the one that never arrives.
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

// maxPerPass bounds how many account-wide or directory-wide events one pass
// will speak about. A burst of transitions usually means several buckets
// crossed at once and the reader needs the worst of them, not all of them; the
// rest stay in the log where `bloodhound events` can show them.
//
// Session-scoped events are outside this cap and capped by their own nature
// instead: they reach exactly one conversation, one line per kind, so a
// machine watching a dozen sessions cannot crowd the pressure warnings out of
// a pass.
const maxPerPass = 3

// backfillLimit bounds what a first run, or a run after a long gap, will look
// at. Without it a machine that has been collecting for months would announce
// its entire history the first time this is switched on.
const backfillLimit = 25

// armHorizon is how far out a reset may be and still be worth promising a
// wakeup for.
//
// A five hour window always qualifies, which is the case the feature exists
// for. A weekly window usually does not, and that is deliberate: "I will write
// to you on Thursday" is not resuming work, it is a calendar entry, and the
// session it wakes will have been closed for days. Past the horizon nothing is
// promised and nothing is suggested, because there is no honest version of
// either.
const armHorizon = 12 * time.Hour

// resumeGrace is how long after the window reopens a promise is still worth
// keeping. Past it the wakeup would arrive with a stale reason attached, so
// the promise is retired unkept rather than delivered late.
const resumeGrace = 6 * time.Hour

// projectionHorizon is how far out a projected crossing may be and still be
// worth interrupting someone about.
//
// The projection itself is honest and stays in the log and on the dashboard
// whatever this is set to. What it is not is a claim about tomorrow. The burn
// rate behind it is measured over the last hour, and extrapolating one hour of
// work across two days assumes the machine keeps working through the night at
// the pace it happens to be going right now. On a weekly window that produces
// exactly the message this was written after: a meter at 56% announcing a cap
// it will reach in a day and three quarters, which is neither wrong nor
// something a reader can act on.
//
// Six hours is where the extrapolation stops being a leap. It is longer than
// the burn window it is built from, short enough that the current pace is
// still a fair description of the near future, and long enough to finish what
// is in flight and write it up. Inside a 5h window every crossing qualifies by
// construction, which is right: that window cannot produce a crossing further
// out than its own length.
const projectionHorizon = 6 * time.Hour

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
	// A session whose recent turns have outgrown its own average, which is
	// the run-up to a compaction. Opt-in per directory, and scoped to the one
	// session it is about.
	"recommendation": true,
	// A session's prompt cache in its last stretch. Opt-in per directory, and
	// the only thing bloodhound will say to a session that is not working.
	"cache": true,
}

// pressureKind reports whether a kind is one of the warnings a directory does
// not get to switch off, as opposed to the two opt-in nudges. The distinction
// decides three things: whether the cap applies, whether a project's pressure
// wording applies, and whether a wakeup line belongs underneath.
func pressureKind(kind string) bool {
	switch kind {
	case "budget", "limit_projection", "saturation":
		return true
	}
	return false
}

// listSessions reads the machine's session registry. A variable so a test can
// supply a machine that does not exist; production never replaces it, and
// nothing outside this package can.
var listSessions = ccsock.ListSessions

// Stats is what one pass did, for the daemon log.
type Stats struct {
	Considered int
	Announced  int
	Delivered  int
	// Armed counts sessions noted down for a wakeup this pass, and Resumed
	// counts promises kept.
	Armed   int
	Resumed int
	Skipped map[string]int
	Errors  []string
}

// Options configure a pass. The zero value is the normal one.
type Options struct {
	Now time.Time
	// DryRun resolves and gates everything but sends nothing, and does not
	// advance the cursor, arm a wakeup, or resolve one. What `bloodhound
	// budget announce --dry-run` uses.
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
// and concerned, then keeps any wakeup promise that has come due.
func Run(ctx context.Context, s *store.Store, opt Options) (Stats, error) {
	st := Stats{Skipped: map[string]int{}}
	now := opt.Now
	if now.IsZero() {
		now = time.Now()
	}

	// Promises first, and outside the event pass entirely.
	//
	// A wakeup comes due because a clock passed a moment, not because
	// anything was appended to the log, so it cannot hang off the cursor. It
	// also has to survive every early return below: the pass where nothing
	// transitioned is exactly the pass where a five hour window quietly
	// reopened with nobody working.
	if err := deliverResumes(ctx, s, now, opt, &st); err != nil {
		st.Errors = append(st.Errors, fmt.Sprintf("wakeups: %v", err))
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

	worth := capped(filterWorthSaying(evs, now), &st)
	st.Announced = len(worth)

	if len(worth) > 0 {
		if err := deliver(ctx, s, now, worth, opt, &st); err != nil {
			st.Errors = append(st.Errors, err.Error())
		}
	}
	return st, nil
}

// capped applies maxPerPass to the events that go to everyone, and leaves the
// session-scoped ones alone.
//
// Two groups because one cap over both would let a busy machine's session
// nudges push out the pressure warnings, and the two are not competing for the
// same reader anyway: a session-scoped event reaches exactly one conversation,
// where it is at most one line.
func capped(evs []events.Event, st *Stats) []events.Event {
	var broad, scoped []events.Event
	for _, e := range evs {
		if e.Scope.Session != "" {
			scoped = append(scoped, e)
			continue
		}
		broad = append(broad, e)
	}
	if len(broad) > maxPerPass {
		st.Skipped["not the most recent transition"] += len(broad) - maxPerPass
		broad = broad[len(broad)-maxPerPass:]
	}
	return append(broad, scoped...)
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
func filterWorthSaying(evs []events.Event, now time.Time) []events.Event {
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
			// Near enough to be believed, not merely true. See
			// projectionHorizon: a crossing two days out is a fact about an
			// extrapolation rather than a fact about the week.
			if state == "projected" && crossingIsNear(e, now) {
				out = append(out, e)
			}
		case "saturation":
			if state == "saturated" {
				out = append(out, e)
			}
		case "recommendation":
			// "watch" and "ok" are dashboard states. Only the one that says a
			// compaction is coming is worth a line in a conversation.
			if state == "compact" {
				out = append(out, e)
			}
		case "cache":
			// "expiring" is the only actionable band: the cache is still warm,
			// so a turn started now is cheap, and it is about to stop being
			// so. "expired" is a bill already paid and "warm" is nothing at
			// all.
			if state == "expiring" {
				out = append(out, e)
			}
		}
	}
	return out
}

// crossingIsNear reports whether a projected crossing is close enough to
// interrupt someone about.
//
// Fails open. A projection whose ETA did not survive the round trip through
// the log is announced rather than dropped: the sensor said the meter is on
// pace to cap out before the window resets, and that is worth hearing even
// when the moment it lands cannot be pinned down.
func crossingIsNear(e events.Event, now time.Time) bool {
	at, ok := resetAt(e.Detail["eta_ts"], now)
	if !ok {
		return true
	}
	return at.Sub(now) <= projectionHorizon
}

// forAccount keeps the events a session on acct should hear: its own
// account's meter facts and everything that is not about a meter at all.
func forAccount(evs []events.Event, acct int64) []events.Event {
	out := make([]events.Event, 0, len(evs))
	for _, e := range evs {
		if e.Scope.Account == 0 || e.Scope.Account == acct {
			out = append(out, e)
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
	sessions, err := listSessions()
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

	sender := opt.Sender
	if sender == nil {
		sender = notify.New()
	}

	// Read once per account for the whole pass rather than per session: the
	// refusal half is an account fact, and re-asking it for every
	// conversation on the machine would let two sessions on the same account
	// be told contradictory things about the same window. An error here
	// costs the qualifier, not the warning: the zero verdict simply claims
	// nothing.
	quotas := map[int64]store.QuotaVerdict{}
	quotaFor := func(acct int64) store.QuotaVerdict {
		if q, ok := quotas[acct]; ok {
			return q
		}
		q, qerr := s.QuotaNow(ctx, acct, now.UnixMilli())
		if qerr != nil {
			st.Errors = append(st.Errors, fmt.Sprintf("quota verdict (account %d): %v", acct, qerr))
		}
		quotas[acct] = q
		return q
	}

	for _, sess := range sessions {
		cfg := configFor(sess.CWD)
		// A session hears about its own account's meter and nobody else's:
		// a projection on the work account is not a warning for a
		// conversation spending the personal one.
		acct, aerr := s.AccountForSession(ctx, sess.SessionID)
		if aerr != nil {
			st.Errors = append(st.Errors, fmt.Sprintf("%s: account: %v", sess.SessionID, aerr))
			continue
		}
		msg := compose(forAccount(evs, acct), sess, now, cfg, quotaFor(acct))
		if msg.Text == "" {
			// Nothing in this batch concerns this session. Not a skip: there
			// was never anything to deliver, so counting it would make the log
			// read as if the gate had refused a warning.
			continue
		}

		// The gate is chosen by what is in the message rather than set once
		// for the pass. A cache nudge is the one line worth waking a resting
		// session for, and once that wakeup is being paid for anyway the rest
		// of the batch rides along at no extra cost.
		g := gate
		g.AllowAtRest = msg.CacheNudge
		if d := g.Admit(sess); !d.Admit {
			st.Skipped[d.Skip]++
			continue
		}

		if opt.DryRun {
			st.Delivered++
			if msg.Arm != nil {
				st.Armed++
			}
			continue
		}
		if _, err := sender.Send(ctx, notify.Target{SessionID: sess.SessionID}, msg.Text); err != nil {
			if notify.Undeliverable(err) {
				st.Skipped[notify.SkipUnreachable]++
				continue
			}
			st.Errors = append(st.Errors, fmt.Sprintf("%s: %v", sess.SessionID, err))
			continue
		}
		st.Delivered++
		for _, p := range msg.problems {
			st.Errors = append(st.Errors, fmt.Sprintf("%s: %s", sess.SessionID, p))
		}

		// Arming after the send, never before. The promise is only worth
		// keeping if the session heard the stop that motivated it; waking a
		// conversation that was never told anything, hours later, with "the
		// window has reopened" is the kind of message that gets a tool
		// uninstalled.
		if msg.Arm != nil {
			if err := arm(ctx, s, sess, now, *msg.Arm); err != nil {
				st.Errors = append(st.Errors, fmt.Sprintf("%s: arm wakeup: %v", sess.SessionID, err))
				continue
			}
			st.Armed++
		}
	}
	return nil
}

// armPlan is a promise about to be made: which window, and when it reopens.
type armPlan struct {
	Bucket string
	At     time.Time
	Reason string
}

// message is what one session is about to hear, and what follows from it.
type message struct {
	Text string
	// CacheNudge is set when a cache line made it in, which is what allows
	// this delivery to wake a resting session.
	CacheNudge bool
	// Arm is set when the project asked bloodhound to carry the wakeup and
	// there is a deadline near enough to promise one for.
	Arm *armPlan
	// problems are templates in the project's config that did not render.
	// Collected rather than raised: the built-in wording went out in their
	// place, so this is something to log, never something to fail on.
	problems []string
}

// compose builds what one session should hear, or the zero message when none
// of the batch concerns it.
//
// A budget event is directory-scoped, so it goes only to sessions running in
// that directory: telling an unrelated project that someone else's allowance
// is tight is noise, and it is the mistake a global broadcast makes. The
// limit events are account-wide and go to everyone admitted, because the 5h
// cliff stops every session on the machine, not just the one that caused it.
//
// The order of the finished text is pressure, then the wakeup line that
// belongs to it, then the opt-in nudges, then the project's own closing line.
// Facts first and advice last, so a message that gets skimmed is skimmed in
// the useful direction.
func compose(evs []events.Event, sess ccsock.Session, now time.Time, cfg projectconfig.Config, quota store.QuotaVerdict) message {
	var msg message
	var pressure, nudges []string

	// The soonest reset among the lines that made it in, and which bucket it
	// belongs to. Two buckets crossing together is one situation with two
	// deadlines, and anything said underneath has to name which of them it
	// means.
	var soonest time.Time
	soonestBucket, soonestReason := "", ""
	// The newest transition in the batch, which is what a project's own
	// wording is rendered against when several arrived at once.
	var newest events.Event

	for _, e := range evs {
		if e.Scope.Cwd != "" && !sameDir(e.Scope.Cwd, sess.CWD) {
			continue
		}
		// A session-scoped event is about one conversation and is meaningless
		// in any other, however interested its neighbours might be.
		if e.Scope.Session != "" && e.Scope.Session != sess.SessionID {
			continue
		}
		if !wanted(e, cfg) {
			continue
		}
		line, err := lineFor(e, sess, now, cfg, quota)
		if line == "" {
			continue
		}
		if err != nil {
			// The wording was the project's and it did not render, so the
			// built-in line went out instead. Worth saying once in the daemon
			// log; never worth dropping the message over.
			msg.wordingProblem(e, err)
		}

		kind, _, _ := splitKind(e.Kind)
		newest = e
		if pressureKind(kind) {
			pressure = append(pressure, line)
			if at, ok := resetAt(e.Detail["reset_ts"], now); ok && (soonest.IsZero() || at.Before(soonest)) {
				soonest, soonestBucket = at, e.Scope.Bucket
				soonestReason = line
			}
			continue
		}
		if kind == "cache" {
			msg.CacheNudge = true
		}
		nudges = append(nudges, line)
	}

	if len(pressure) == 0 && len(nudges) == 0 {
		return message{}
	}

	var out []string
	if len(pressure) > 0 {
		body := strings.Join(pressure, "\n")
		if cfg.Pressure.Message != "" {
			var err error
			body, err = projectconfig.Render(cfg.Pressure.Message,
				varsFor(newest, sess, now, body, soonest, soonestBucket), body)
			if err != nil {
				msg.wordingProblem(newest, err)
			}
		}
		out = append(out, body)

		if line, plan := wakeupLine(cfg, sess, now, soonest, soonestBucket, soonestReason, quota); line != "" {
			out = append(out, line)
			msg.Arm = plan
		}
	}
	out = append(out, nudges...)

	msg.Text = strings.Join(out, "\n")
	return msg
}

// wordingProblem is recorded rather than raised. Collected here so the one
// caller that cares (the daemon log) has something to print without compose
// having to carry an error return through every branch.
func (m *message) wordingProblem(e events.Event, err error) {
	m.problems = append(m.problems, fmt.Sprintf("%s: %v", e.Kind, err))
}

// wakeupLine renders the far side of a warning, and reports the promise it
// implies.
//
// Three outcomes rather than two. Off says nothing. Nudge says the work could
// pick up again if something is armed to wake it, and promises nothing.
// Resume promises, and only when there is a deadline close enough to keep the
// promise honest: past armHorizon, or with no reset in the text at all,
// bloodhound has nothing it can commit to and says so by saying nothing.
func wakeupLine(cfg projectconfig.Config, sess ccsock.Session, now time.Time, at time.Time, bucket, reason string, quota store.QuotaVerdict) (string, *armPlan) {
	if bucket == "" || at.IsZero() {
		return "", nil
	}
	// Both forms of this line, and the promise underneath them, start from
	// "work that stops here". A session already running at the lower request
	// priority is not stopping: its requests are still being served, slower,
	// until the window it is standing in for reopens. Offering to wake it
	// then would be waking a conversation that never paused, and the nudge
	// form is worse than useless — it tells someone who is working that they
	// are about to stop.
	if quota.LowPriority(sess.SessionID) {
		return "", nil
	}
	v := projectconfig.Vars{
		Kind:    "wakeup",
		Bucket:  budget.BucketLabel(bucket),
		Cwd:     sess.CWD,
		Dir:     budget.Label(sess.CWD),
		Session: sess.SessionID,
		Reset:   whenfmt.Phrase(at, now),
		ResetAt: at,
		Now:     now,
	}

	switch {
	case cfg.Wakeup.Suggests():
		// Offered rather than ordered, and it names no mechanism. Bloodhound
		// does not arm anything here and has no idea what the reader schedules
		// wakeups with; what it knows is that the work is about to stop and
		// when it could start again, which is the part worth saying out loud.
		//
		// It names the bucket rather than repeating the time, which is already
		// in the line above it, and rather than saying "that reset", which is
		// ambiguous on the batch where both windows crossed at once.
		fallback := fmt.Sprintf(
			"Work that stops here could pick up again when the %s window reopens, if something is armed to wake it.",
			v.Bucket)
		v.Text = fallback
		line, _ := projectconfig.Render(cfg.Wakeup.NudgeMessage, v, fallback)
		return line, nil

	case cfg.Wakeup.Arms():
		if at.Sub(now) > armHorizon {
			return "", nil
		}
		fallback := fmt.Sprintf(
			"Work that stops here can be picked up again: bloodhound will write to this session when the %s window reopens %s.",
			v.Bucket, v.Reset)
		v.Text = fallback
		line, _ := projectconfig.Render(cfg.Wakeup.ArmedMessage, v, fallback)
		return line, &armPlan{Bucket: bucket, At: at, Reason: reason}
	}
	return "", nil
}

// arm records the promise.
func arm(ctx context.Context, s *store.Store, sess ccsock.Session, now time.Time, plan armPlan) error {
	_, err := s.ArmWakeup(ctx, store.SessionWakeup{
		SessionUUID: sess.SessionID,
		PID:         sess.PID,
		Cwd:         sess.CWD,
		Bucket:      plan.Bucket,
		Reason:      plan.Reason,
		ArmedMS:     now.UnixMilli(),
		DueMS:       plan.At.UnixMilli(),
		ExpireMS:    plan.At.Add(resumeGrace).UnixMilli(),
	})
	return err
}

// deliverResumes keeps the promises that have come due.
//
// This is the one delivery bloodhound makes that nobody is awake for, and the
// only one where a cold resume is the intended outcome rather than the thing
// being avoided. Everything else here is about not keeping a promise badly:
// not waking a session into the same wall it stopped at, not waking it so late
// that the reason is stale, and not giving up the first time the socket does
// not answer.
func deliverResumes(ctx context.Context, s *store.Store, now time.Time, opt Options, st *Stats) error {
	pending, err := s.PendingWakeups(ctx)
	if err != nil || len(pending) == 0 {
		return err
	}
	nowMS := now.UnixMilli()

	// A window can turn over earlier than the reading that armed the promise
	// predicted, and the log knows before the clock does. Consulting it costs
	// one query, and only when something is still waiting.
	early := earlyResets(ctx, s, pending, nowMS)

	// The latest deadline still ahead of us, per session. A session warned
	// about both windows must not be woken by the 5h reopening while the week
	// is still shut: that hands it back the wall it stopped at, at the cost of
	// the whole prefix. The later promise carries it instead.
	stillShut := map[string]int64{}
	for _, w := range pending {
		if w.DueMS > nowMS && !early[w.Bucket] && w.DueMS > stillShut[w.SessionUUID] {
			stillShut[w.SessionUUID] = w.DueMS
		}
	}

	var due []store.SessionWakeup
	for _, w := range pending {
		if w.DueMS <= nowMS || early[w.Bucket] {
			due = append(due, w)
		}
	}
	if len(due) == 0 {
		return nil
	}

	// Both windows reopening in the same pass is one event to the session
	// sitting there, not two. The later of them speaks, because it is the one
	// that was actually holding the work up, and the other resolves quietly.
	// Without this a session warned about both gets woken twice in a minute,
	// which is the same cold prefix paid twice over.
	speaks := map[string]int64{}
	for _, w := range due {
		if nowMS > w.ExpireMS {
			continue // a stale promise never speaks, so it never wins the slot
		}
		if cur, ok := speaks[w.SessionUUID]; !ok || w.DueMS > cur {
			speaks[w.SessionUUID] = w.DueMS
		}
	}

	sessions, err := listSessions()
	if err != nil {
		return fmt.Errorf("read session registry: %w", err)
	}
	byUUID := make(map[string]ccsock.Session, len(sessions))
	for _, sess := range sessions {
		byUUID[sess.SessionID] = sess
	}

	sender := opt.Sender
	if sender == nil {
		sender = notify.New()
	}

	for _, w := range due {
		resolve := func(outcome, note string) {
			if opt.DryRun {
				return
			}
			if err := s.ResolveWakeup(ctx, w.ID, outcome, nowMS, note); err != nil {
				st.Errors = append(st.Errors, fmt.Sprintf("wakeup %d: %v", w.ID, err))
			}
		}

		if nowMS > w.ExpireMS {
			resolve(store.WakeupExpired, "nobody reachable before the reason went stale")
			st.Skipped["wakeup went stale"]++
			continue
		}
		if until, ok := stillShut[w.SessionUUID]; ok && until > w.DueMS {
			resolve(store.WakeupSuperseded, "another window this session was warned about is still shut")
			st.Skipped["wakeup superseded by a later window"]++
			continue
		}
		if speaks[w.SessionUUID] != w.DueMS {
			resolve(store.WakeupSuperseded, "a later window reopened in the same pass and carries this")
			st.Skipped["wakeup superseded by a later window"]++
			continue
		}

		sess, ok := byUUID[w.SessionUUID]
		if !ok {
			// The session is gone from the registry, which usually means it
			// was closed. Kept pending rather than retired: a registry read
			// during a restart can miss a session that is about to be back,
			// and expire_ms is what eventually ends the waiting.
			if !opt.DryRun {
				_ = s.TouchWakeup(ctx, w.ID, "session not in the registry")
			}
			st.Skipped["wakeup target is gone"]++
			continue
		}

		// Everything the gate normally refuses is deliberately allowed here.
		// What remains is the one question left: is anything listening.
		g := notify.Gate{Now: now, AllowAtRest: true, AllowCold: true}
		if opt.Gate != nil {
			g = *opt.Gate
			g.AllowAtRest, g.AllowCold = true, true
		}
		if d := g.Admit(sess); !d.Admit {
			if !opt.DryRun {
				_ = s.TouchWakeup(ctx, w.ID, d.Skip)
			}
			st.Skipped[d.Skip]++
			continue
		}

		text := resumeText(w, sess, now, configFor(w.Cwd))
		if opt.DryRun {
			st.Resumed++
			continue
		}
		if _, err := sender.Send(ctx, notify.Target{SessionID: sess.SessionID}, text); err != nil {
			if notify.Undeliverable(err) {
				_ = s.TouchWakeup(ctx, w.ID, notify.SkipUnreachable)
				st.Skipped[notify.SkipUnreachable]++
				continue
			}
			st.Errors = append(st.Errors, fmt.Sprintf("wakeup %d: %v", w.ID, err))
			continue
		}
		resolve(store.WakeupDelivered, "")
		st.Resumed++
	}
	return nil
}

// earlyResets reports which of the buckets still waiting have already turned
// over according to the log.
//
// Best effort by design. The deadline stored on the promise is the primary
// trigger and needs nothing but a clock; this only brings a wakeup forward
// when a poll saw the window turn over sooner than the reading that armed it
// predicted. A failure here means the promise fires on its own deadline
// instead, which is the answer it would have had anyway.
func earlyResets(ctx context.Context, s *store.Store, pending []store.SessionWakeup, nowMS int64) map[string]bool {
	oldest := int64(0)
	for _, w := range pending {
		if w.DueMS > nowMS && (oldest == 0 || w.ArmedMS < oldest) {
			oldest = w.ArmedMS
		}
	}
	if oldest == 0 {
		return nil
	}
	evs, err := events.Query(ctx, s.DB, events.Filter{
		Kinds:   []string{"window.reset"},
		SinceMS: oldest,
		Limit:   50,
	})
	if err != nil || len(evs) == 0 {
		return nil
	}
	out := map[string]bool{}
	for _, e := range evs {
		if e.Scope.Bucket != "" {
			out[e.Scope.Bucket] = true
		}
	}
	return out
}

// resumeText is what arrives on the far side of a window.
//
// Written to read cold, because by definition it does. The session it reaches
// has been sitting at its prompt for hours, its context may have been rebuilt
// on the way in, and nobody is watching the terminal; a line that only makes
// sense to someone who remembers the warning is a line that lands as noise.
// So it says what reopened, what stopped, and how long ago.
func resumeText(w store.SessionWakeup, sess ccsock.Session, now time.Time, cfg projectconfig.Config) string {
	bucket := budget.BucketLabel(w.Bucket)
	waited := whenfmt.Dur(now.Sub(w.Armed()).Milliseconds())

	fallback := fmt.Sprintf("The %s window has reopened. Work here stopped %s ago under the pressure bloodhound flagged then, and can pick up again now.",
		bucket, waited)
	if w.Reason != "" {
		fallback = fmt.Sprintf("The %s window has reopened, so work here can pick up again. What stopped it %s ago: %s",
			bucket, waited, w.Reason)
	}

	v := projectconfig.Vars{
		Text:    fallback,
		Kind:    "resume",
		Bucket:  bucket,
		Cwd:     w.Cwd,
		Dir:     budget.Label(w.Cwd),
		Session: sess.SessionID,
		ArmedAt: w.Armed(),
		Waited:  waited,
		Now:     now,
	}
	text, _ := projectconfig.Render(cfg.Wakeup.ResumeMessage, v, fallback)
	return text
}

// wanted reports whether an opt-in event may be said to this session at all.
//
// The pressure events are unconditional: a limit that stops every session on
// the machine is not something a directory gets to switch off. The two nudges
// are different in kind. Nothing is going wrong when either fires, and they
// are advice about how to work rather than facts about the meter, so they go
// only where they were asked for.
func wanted(e events.Event, cfg projectconfig.Config) bool {
	kind, _, ok := splitKind(e.Kind)
	if !ok {
		return true
	}
	switch kind {
	case "recommendation":
		return cfg.WriteupNudge.Enabled
	case "cache":
		return cfg.CacheNudge.Enabled
	}
	return true
}

// configFor asks the directory a session is working in what it wants to hear.
//
// Silent on every failure. A directory with no file, an unreadable one, or a
// session with no cwd at all all mean the same thing here: nobody asked for
// the extra lines, so they are left out.
func configFor(cwd string) projectconfig.Config {
	if cwd == "" {
		return projectconfig.Default()
	}
	cfg, _, err := projectconfig.Load(cwd)
	if err != nil {
		return projectconfig.Default()
	}
	return cfg
}

func sameDir(a, b string) bool {
	na, err1 := budget.NormalizeCwd(a)
	nb, err2 := budget.NormalizeCwd(b)
	if err1 != nil || err2 != nil {
		return a == b
	}
	return na == nb
}

// lineFor renders one event, in the project's words when it asked for its own.
//
// The error is returned alongside the line rather than instead of it: a
// template that failed still yields the built-in sentence, and the caller
// logs the failure without anybody losing a warning over it.
func lineFor(e events.Event, sess ccsock.Session, now time.Time, cfg projectconfig.Config, quota store.QuotaVerdict) (string, error) {
	base := describe(e, now, quota, sess.SessionID)
	if base == "" {
		return "", nil
	}
	kind, _, _ := splitKind(e.Kind)

	tmpl := ""
	switch kind {
	case "recommendation":
		tmpl = cfg.WriteupNudge.Message
	case "cache":
		tmpl = cfg.CacheNudge.Message
	}
	if tmpl == "" {
		return base, nil
	}
	at, _ := resetAt(e.Detail["reset_ts"], now)
	return projectconfig.Render(tmpl, varsFor(e, sess, now, base, at, e.Scope.Bucket), base)
}

// varsFor is what a project's templates see. The event supplies the facts, the
// session supplies who is being told, and Text supplies what bloodhound would
// have said, so a template can reframe without reproducing.
func varsFor(e events.Event, sess ccsock.Session, now time.Time, text string, at time.Time, bucket string) projectconfig.Vars {
	kind, state, _ := splitKind(e.Kind)
	v := projectconfig.Vars{
		Text:    text,
		Kind:    kind,
		State:   state,
		Bucket:  budget.BucketLabel(bucket),
		Cwd:     sess.CWD,
		Dir:     budget.Label(sess.CWD),
		Project: e.Scope.Project,
		Session: sess.SessionID,
		ETA:     when(e.Detail["eta_ts"], now),
		Now:     now,
	}
	if pct, ok := e.Detail["pct"].(float64); ok {
		v.Pct = int(pct)
	}
	if !at.IsZero() {
		v.ResetAt = at
		v.Reset = whenfmt.Phrase(at, now)
	}
	return v
}

// describe renders one event as a sentence a reader can act on.
//
// Phrased as an observation, never an instruction. Bloodhound measures; what
// to do about the measurement is the reader's call, and a monitoring tool that
// starts issuing orders into conversations is one users turn off.
func describe(e events.Event, now time.Time, quota store.QuotaVerdict, sessionUUID string) string {
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
		// Built from parts rather than a table of format strings, because
		// every one of the four facts is separately absent on some install and
		// the sentence has to read as English without any of them. Where it
		// starts from matters as much as where it is heading: "on pace to cap
		// out" reads very differently at 91% than at 56%, and a reader given
		// only the projection has to go and look the reading up before they
		// can judge it.
		line := fmt.Sprintf("The %s meter", b)
		if pct, ok := e.Detail["pct"].(float64); ok {
			line += fmt.Sprintf(" is at %d%%", int(pct))
			if burn, ok := e.Detail["burn_pct_per_h"].(float64); ok && burn > 0 {
				line += fmt.Sprintf(", rising about %s/h", pctFigure(burn))
			}
			line += ", and is"
		} else {
			line += " is"
		}
		line += " on pace to reach 100%"
		if eta := when(e.Detail["eta_ts"], now); eta != "" {
			line += " " + eta
		}
		if reset != "" {
			return line + fmt.Sprintf(", before it resets %s.", reset)
		}
		return line + ", before it resets."
	case "recommendation":
		line := "This session's recent turns have outgrown its own average, which is the run-up to a compaction."
		if reason, _ := e.Detail["reason"].(string); reason != "" {
			line = "This session: " + reason + "."
		}
		// The two costs the recommendation is actually a comparison between.
		// Without them the line is an opinion; with them it is the arithmetic
		// that produced the opinion, which is what lets a reader disagree with
		// it on a session where they know better.
		if cost, ok := e.Detail["compact_cost_pct"].(float64); ok && cost > 0 {
			line += fmt.Sprintf(" Compacting costs about %s of a context window", pctFigure(cost))
			if cold, ok := e.Detail["cold_resume_pct"].(float64); ok && cold > 0 {
				line += fmt.Sprintf(", against %s to resume this session cold", pctFigure(cold))
			}
			line += "."
		}
		// The last sentence is the whole point of saying it early. After the
		// compaction the detail is gone and the writeup has to be rebuilt from
		// a summary; before it, the detail is still sitting in the context.
		return line + " Writing up where things stand costs less now than reconstructing it afterwards."
	case "cache":
		// Deliberately not phrased as an emergency. Nothing is wrong: a cache
		// lapsing is the normal end of an idle stretch, and the only reason to
		// mention it is that writing something down while the context is still
		// loaded is cheaper than reconstructing it from a cold start later.
		line := "This session's prompt cache is in its last stretch"
		if in, ok := e.Detail["expires_in_s"].(float64); ok && in > 0 {
			line += fmt.Sprintf(", about %s from lapsing", whenfmt.Dur(int64(in)*1000))
		}
		line += ". Writing down where things stand now is paid at warm-cache rates"
		if cold, ok := e.Detail["cold_resume_pct"].(float64); ok && cold > 0 {
			line += fmt.Sprintf("; after it lapses the same summary starts by rebuilding the whole context, about %s of one", pctFigure(cold))
			return line + "."
		}
		return line + "; after it lapses the same summary starts by rebuilding the whole context."
	case "saturation":
		at := "its cap"
		if pct, ok := e.Detail["pct"].(float64); ok {
			at = fmt.Sprintf("%d%%", int(pct))
		}
		line := fmt.Sprintf("The %s meter has stopped moving at %s, so every figure downstream is now an estimate.", b, at)
		if reset != "" {
			line += fmt.Sprintf(" It resets %s.", reset)
		}
		if clause := quotaClause(quota, sessionUUID, e.Scope.Bucket); clause != "" {
			line += " " + clause
		}
		return line
	}
	return ""
}

// quotaClause says what the meter stopping actually means, which the meter
// itself cannot.
//
// Without it a saturation line says a number stopped moving and leaves the
// reader to guess between three situations: spend billing to the pay-per-use
// tier, requests refused outright, and requests still being served at the
// lower priority Claude Code offers when the five hour window closes. Guessing
// wrong in the cautious direction is not free. A session told only that the
// meter is pinned reads it as a stop, writes up where it got to and waits,
// while the requests it would have made were going through the whole time.
// That is the failure this clause exists to prevent, and it is why the
// low-priority case is phrased around what still works rather than what
// does not.
//
// Empty whenever there is nothing on record, which is most of the time. The
// sentence above it is written to stand alone.
func quotaClause(v store.QuotaVerdict, sessionUUID, bucket string) string {
	if !v.Refused && !v.LowPriorityActive {
		return ""
	}
	about := v.Bucket
	if about == "" {
		about = "session"
	}
	if bucket != "" && bucket != about {
		return ""
	}

	if v.LowPriority(sessionUUID) {
		return "This session is running at the lower request priority until then, so requests are still going through: they may wait for spare capacity, and they draw on the weekly allowance rather than this window."
	}
	if v.UsingOverage {
		return "Spend past the cap is billing to the pay-per-use extra usage tier rather than being refused."
	}
	if !v.Refused {
		return ""
	}

	line := "Requests are being refused rather than billed to extra usage"
	if v.OverageDisabledReason != "" {
		line += fmt.Sprintf(" (%s)", strings.ReplaceAll(v.OverageDisabledReason, "_", " "))
	}
	line += "."
	if v.LowPriorityOffered {
		// The one place bloodhound comes close to naming a remedy, and it is
		// still an observation: the offer is Claude Code's, already on the
		// reader's screen, and what is being said is that it exists and what
		// it costs. Someone who does not know it is there reads a refusal as
		// the end of the window.
		line += " Claude Code offers to carry on at a lower request priority instead, against the weekly allowance."
	}
	return line
}

// pctFigure renders a percentage the way the rest of bloodhound's prose does:
// no decimal once the number is big enough that a tenth is noise, one below
// that so a slow burn does not print as a flat zero.
func pctFigure(v float64) string {
	if v >= 10 {
		return fmt.Sprintf("%.0f%%", v)
	}
	return fmt.Sprintf("%.1f%%", v)
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
	out := make(map[string]notify.CacheState, len(sessions))
	for _, sess := range sessions {
		if sess.SessionID == "" {
			continue
		}
		ref, err := sessioninsight.BySessionUUID(ctx, s.DB, sess.SessionID)
		if err != nil || ref == nil {
			continue // never ingested; the gate reads absence as unknown
		}
		// Priced in the session's own account's points: each meter has its
		// own calibration.
		acct, _ := s.AccountForSession(ctx, sess.SessionID)
		tokensPerPctCW, _, _, hasCal, _ := s.LatestCalibrationMedian(ctx, acct, "session", 10)
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
