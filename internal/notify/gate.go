package notify

import (
	"fmt"
	"time"

	ccsock "github.com/PeterSR/claude-code-socket-transport"
)

// The gate decides which sessions may be told something.
//
// Bloodhound can now reach into a conversation, and the whole difficulty is
// that reaching in is not free. A message delivered to a session sitting at
// its prompt starts a turn that would not otherwise have happened. If that
// session's cache has gone cold, the turn re-pays the entire conversation
// prefix as cache creation before it reads a word of what we said. On a long
// session that is a genuinely expensive way to deliver a sentence nobody
// asked for, and it is charged to the very budget the message is warning
// about.
//
// So the rule is inverted from the obvious one. The question is not "who
// would benefit from knowing" but "who is already awake". A session that is
// mid-turn is going to make another model call regardless; slipping a line in
// between its tool calls costs the tokens of the line and nothing else. Every
// other session waits, and if it never wakes it never hears. Missing a warning
// is a small, recoverable loss. Paying a cold resume to deliver one is not,
// and it is the exact failure the tool exists to prevent.
//
// Both of the constraints this was built for fall out of that single rule. A
// cold session is never woken, because a cold session is by definition not
// mid-turn. And a session idling with a warm cache is left alone too, which is
// the right call for a different reason: telling something that is doing
// nothing about budget pressure it is not creating is noise.

// Session activity as Claude Code reports it in the registry. Measured on a
// live machine: "shell" is the resting state and runs to eight days stale,
// while "busy" tracks a session actually working. The other two are documented
// by the transport and are both at-rest states.
const (
	statusBusy    = "busy"
	statusShell   = "shell"
	statusIdle    = "idle"
	statusWaiting = "waiting"
)

// MaxStatusAge is how stale a "busy" reading may be and still be believed.
//
// A turn can legitimately run a long time behind a slow tool call, so this is
// generous. What it guards against is a session that stopped updating its
// status without closing its socket: the process answers, still claims to be
// busy, and has in fact been parked for hours. Delivering there is precisely
// the cold wakeup this gate exists to prevent, so an unbelievably old "busy"
// is treated as at-rest.
const MaxStatusAge = 15 * time.Minute

// Skip reasons, in the order the gate checks them.
const (
	SkipNoInbox     = "no inbox"
	SkipUnreachable = "unreachable"
	SkipAtRest      = "at rest"
	SkipStaleStatus = "status too stale to believe"
	SkipCacheCold   = "cache is cold"
)

// Decision is the gate's answer for one session.
type Decision struct {
	Session ccsock.Session
	// Admit is true when this session may be told.
	Admit bool
	// Skip is why not, empty when admitted. Worth logging in aggregate: a run
	// where every session was skipped for "at rest" is the gate working, not
	// a delivery failure.
	Skip string
}

// CacheState is what bloodhound knows about a session's cache from its own
// transcripts, which the registry cannot tell us.
type CacheState struct {
	// Cold is true when the prefix would have to be re-paid on the next turn.
	Cold bool
	// ColdResumeCostCWTokens is what that would cost, for the log line.
	ColdResumeCostCWTokens float64
}

// Gate admits sessions that are already awake.
type Gate struct {
	Now time.Time
	// MaxStatusAge overrides the default when non-zero.
	MaxStatusAge time.Duration
	// Cache answers "is this session's cache cold" by session UUID. Nil means
	// bloodhound has nothing to say, in which case the activity rule carries
	// the decision on its own. A session absent from the map is likewise
	// unknown rather than warm.
	Cache map[string]CacheState

	// AllowAtRest drops the "must be mid-turn" rule while keeping every other
	// one, including the cold-cache refusal.
	//
	// There is exactly one message where waking a resting session is the
	// cheap option rather than the expensive one, and it is the reason this
	// field exists: a warm cache inside its last stretch. Delivering there
	// starts a turn at warm-cache rates and stops the prefix being rebuilt
	// from nothing afterwards, so the arithmetic that makes the default rule
	// right is the same arithmetic that makes this exception right. Because
	// the cache check still applies, a session that has already gone cold is
	// refused here as firmly as anywhere else.
	AllowAtRest bool

	// AllowCold drops the cold-cache refusal as well, which leaves only "is
	// there something listening".
	//
	// Reserved for a message the user asked for by name and is waiting on: a
	// promised wakeup when a window reopens. The cold resume is not an
	// accident there, it is the thing being paid for, and refusing it would
	// mean the one delivery someone explicitly opted into is the one that
	// silently never arrives.
	AllowCold bool
}

func (g Gate) maxAge() time.Duration {
	if g.MaxStatusAge > 0 {
		return g.MaxStatusAge
	}
	return MaxStatusAge
}

// Admit decides whether one session may be told.
func (g Gate) Admit(s ccsock.Session) Decision {
	d := Decision{Session: s}

	if s.SocketPath == "" {
		d.Skip = SkipNoInbox
		return d
	}
	if !s.Reachable(probeTimeout) {
		d.Skip = SkipUnreachable
		return d
	}
	if !g.AllowAtRest {
		if s.Status != statusBusy {
			// shell, idle, waiting, or a status the registry never reported.
			// All of them mean the session is not mid-turn, so delivering
			// would start one.
			d.Skip = SkipAtRest
			return d
		}
		if age := g.statusAge(s); age > g.maxAge() {
			d.Skip = fmt.Sprintf("%s (%s)", SkipStaleStatus, age.Round(time.Minute))
			return d
		}
	}
	// The cross-check. A busy session should be warm by construction, so this
	// almost never fires; it is here because "almost never" is not "never" and
	// the cost of being wrong is asymmetric. Bloodhound's own view lags ingest
	// by a poll interval, so it can call a live session cold when it is not,
	// and that error direction is the safe one: a skipped warning costs
	// nothing, a cold wakeup costs the prefix.
	if st, ok := g.Cache[s.SessionID]; ok && st.Cold && !g.AllowCold {
		d.Skip = SkipCacheCold
		return d
	}

	d.Admit = true
	return d
}

func (g Gate) statusAge(s ccsock.Session) time.Duration {
	if s.StatusUpdatedAt.IsZero() {
		// Never reported. Treated as infinitely stale rather than fresh, so a
		// session with no status cannot slip through on a default.
		return 1<<62 - 1
	}
	now := g.Now
	if now.IsZero() {
		now = time.Now()
	}
	if age := now.Sub(s.StatusUpdatedAt); age > 0 {
		return age
	}
	return 0
}

// Admitted runs the gate over every session and returns those that may be
// told, plus a tally of why the rest were not.
func (g Gate) Admitted(sessions []ccsock.Session) (admit []ccsock.Session, skipped map[string]int) {
	skipped = map[string]int{}
	for _, s := range sessions {
		d := g.Admit(s)
		if d.Admit {
			admit = append(admit, s)
			continue
		}
		skipped[d.Skip]++
	}
	return admit, skipped
}
