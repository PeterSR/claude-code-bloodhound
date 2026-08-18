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
	"github.com/PeterSR/claude-code-bloodhound/internal/sessioninsight"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
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

	cursor, err := readCursor(ctx, s)
	if err != nil {
		return st, err
	}

	head, err := events.MaxID(ctx, s.DB)
	if err != nil {
		return st, err
	}
	if cursor == 0 {
		// First run. Start at the head rather than the beginning: nobody wants
		// their first switch-on to be a recital of everything that has ever
		// happened, and the levels are all still readable in the log.
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

	evs, err := events.Query(ctx, s.DB, events.Filter{
		SinceID: cursor,
		Kinds:   kinds,
		Limit:   backfillLimit,
	})
	if err != nil {
		return st, err
	}
	st.Considered = len(evs)

	worth := filterWorthSaying(evs)
	if len(worth) > maxPerPass {
		worth = worth[len(worth)-maxPerPass:]
	}
	st.Announced = len(worth)

	if len(worth) > 0 {
		if err := deliver(ctx, s, now, worth, opt, &st); err != nil {
			st.Errors = append(st.Errors, err.Error())
		}
	}

	if !opt.DryRun {
		if err := writeCursor(ctx, s, head); err != nil {
			return st, err
		}
	}
	return st, nil
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
		text := textFor(evs, sess)
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
func textFor(evs []events.Event, sess ccsock.Session) string {
	var lines []string
	for _, e := range evs {
		if e.Scope.Cwd != "" && !sameDir(e.Scope.Cwd, sess.CWD) {
			continue
		}
		if line := describe(e); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
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
func describe(e events.Event) string {
	kind, state, ok := splitKind(e.Kind)
	if !ok {
		return ""
	}
	switch kind {
	case "budget":
		if reason, _ := e.Detail["reason"].(string); reason != "" {
			return "Budget: " + reason + "."
		}
		return fmt.Sprintf("Budget for %s is %s.", budget.Label(e.Scope.Cwd), state)
	case "limit_projection":
		b := budget.BucketLabel(e.Scope.Bucket)
		if eta, ok := e.Detail["eta_ts"].(string); ok && eta != "" {
			return fmt.Sprintf("The %s meter is on pace to reach 100%% at %s, before it resets.", b, eta)
		}
		return fmt.Sprintf("The %s meter is on pace to reach 100%% before it resets.", b)
	case "saturation":
		return fmt.Sprintf("The %s meter has stopped moving at its cap, so every figure downstream is now an estimate.",
			budget.BucketLabel(e.Scope.Bucket))
	}
	return ""
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
		out[sess.SessionID] = notify.CacheState{
			Cold:                   in.ColdResumeCostCWTokens > 0,
			ColdResumeCostCWTokens: in.ColdResumeCostCWTokens,
		}
	}
	return out
}

func readCursor(ctx context.Context, s *store.Store) (int64, error) {
	v, err := s.GetMeta(ctx, cursorKey)
	if err != nil || v == "" {
		return 0, err
	}
	var id int64
	if _, err := fmt.Sscanf(v, "%d", &id); err != nil {
		return 0, nil
	}
	return id, nil
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
