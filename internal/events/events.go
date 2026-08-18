// Package events is the append-only event log: the types every producer and
// consumer shares, the sensor registry, and the table access underneath.
//
// It deliberately imports neither internal/store nor internal/nowstate, so
// that store can append edges from inside its own transactions without an
// import cycle. Everything that needs those lives in the sensors subpackage,
// which nothing here imports back.
//
// Two producer shapes, decided by one question: does this fact stay true for a
// while, or is it a moment?
//
//   - Stays true (a projected limit, a saturation, a pct band): a level.
//     Register a Sensor. The reconciler owns the memory of what it said last
//     time and emits on transitions, which is what lets read-side
//     recomputations stay pure.
//   - A moment (a window reset, a self-heal starting): an edge. Call AppendTx
//     from the transaction that discovered it, so the fact and the event land
//     together or not at all.
package events

import (
	"context"
	"database/sql"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
)

// Scope is what an event or reading is about. The zero value means the whole
// install.
type Scope struct {
	Bucket  string `json:"bucket,omitempty"`  // "session" | "week" | ""
	Session string `json:"session,omitempty"` // session UUID
	Project string `json:"project,omitempty"` // denormalised so consumers can filter without a join
}

// Event is one recorded transition.
type Event struct {
	ID       int64  `json:"id"`
	TSISO    string `json:"ts"`
	TSUnixMS int64  `json:"ts_unix_ms"`
	Kind     string `json:"kind"`
	// PrevState is what the level transitioned from. Empty for edges, which
	// have no previous state by definition. Present so a consumer acting on a
	// single event never has to reconstruct where it came from, which is what
	// makes replay after a crash idempotent.
	PrevState string         `json:"prev_state,omitempty"`
	Scope     Scope          `json:"scope"`
	Detail    map[string]any `json:"detail,omitempty"`
}

// Level is the current value of one level track.
type Level struct {
	Kind    string `json:"kind"`
	Scope   Scope  `json:"scope"`
	State   string `json:"state"`
	SinceMS int64  `json:"since_ms"`
}

// Reading is what a Sensor reports about one level, right now. The sensor
// never decides whether anything changed.
//
// Two different absences, and the difference matters:
//
//   - Omitting a Reading entirely means "I have nothing to say about this",
//     and leaves any existing level untouched. This is how a session ageing
//     out of the recent window avoids emitting a spurious transition.
//   - Returning State == "" means "I actively do not know", which is recorded
//     as the state "unknown" and does emit a transition. A projected limit
//     that vanishes because /usage extraction broke must not be silently
//     forgotten, or a consumer goes on believing the last thing it heard.
type Reading struct {
	Kind   string
	Scope  Scope
	State  string
	Detail map[string]any
}

// StateUnknown is what an empty Reading.State is recorded as.
const StateUnknown = "unknown"

// World is the read-side handle a Sensor gets. Deliberately narrow: a sensor
// cannot write.
//
// Pool is nowstate.Compute's output, done once per reconcile and shared, so
// twenty sensors do not each drive the same query set. It is nil when the
// caller could not compute it.
type World struct {
	DB   *sql.DB
	Now  time.Time
	Pool *routes.NowResponse

	// TokensPerPctCW converts cost-weighted tokens into percentage points.
	// Shared here for the same reason Pool is: several sensors want it and
	// none of them should be re-deriving it. HasCalibration is false when
	// there is no usable run yet, in which case the pct-denominated fields on
	// an Insight are absent rather than zero.
	TokensPerPctCW float64
	HasCalibration bool
}

// Sensor answers "what is true now" for one family of facts. Pure: the same
// database state in yields the same readings out, and it is called at most
// once per reconcile.
type Sensor struct {
	Name string
	Read func(ctx context.Context, w World) ([]Reading, error)
}

var registry []Sensor

// Register adds a sensor. Called from init() in the sensors subpackage, so
// importing that package for its side effect is what turns sensors on.
func Register(s Sensor) { registry = append(registry, s) }

// Registered returns the sensors in registration order.
func Registered() []Sensor { return registry }

// SetRegistry replaces the registry wholesale. Only for tests, which need a
// known set of sensors rather than whatever happens to have been imported.
func SetRegistry(s []Sensor) { registry = s }

// Kind is the wire name for a level in a given state: "limit_projection"
// plus "projected" is "limit_projection.projected". Keeping the join in one
// function means the registry never has to spell out paired names.
func Kind(family, state string) string {
	if state == "" {
		state = StateUnknown
	}
	return family + "." + state
}
