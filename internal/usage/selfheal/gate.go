package selfheal

import (
	"errors"
	"sync"
	"time"
)

// gate enforces two invariants across all callers of Run:
//
//   1. **Mutex**: at most one self-heal in flight. The daemon's
//      runPollOnce and the /api/extractor/retrain handler can both call
//      Run; without serialisation they'd race on claude -p + the
//      extractor file.
//
//   2. **Cool-down**: after several failed heals back-to-back, skip
//      automatic heals for a stretch instead of burning a turn on
//      every polling cycle. Manual retrains via the API still bypass
//      the cool-down — the user explicitly asked.
var gate = &healGate{
	maxConsecutiveFails: 3,
	cooldown:            15 * time.Minute,
}

type healGate struct {
	mu sync.Mutex

	running bool

	consecutiveFails    int
	lastFailAt          time.Time
	maxConsecutiveFails int
	cooldown            time.Duration
}

// ErrAlreadyRunning indicates a heal is in flight; the caller should
// not start another.
var ErrAlreadyRunning = errors.New("self-heal already running")

// ErrCoolingDown indicates we've recently had several heal failures
// and are deliberately pausing automatic attempts. Manual paths can
// bypass via tryAcquire(force=true).
var ErrCoolingDown = errors.New("self-heal in cool-down after consecutive failures")

// tryAcquire claims the heal slot. Returns nil on success; the caller
// must invoke release(success bool) exactly once when done.
// force=true bypasses the cool-down (manual retrains do this).
func (g *healGate) tryAcquire(force bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.running {
		return ErrAlreadyRunning
	}
	if !force && g.consecutiveFails >= g.maxConsecutiveFails {
		if time.Since(g.lastFailAt) < g.cooldown {
			return ErrCoolingDown
		}
		// Cool-down expired; let the next attempt go.
	}
	g.running = true
	return nil
}

// release marks the heal slot free. Pass success=true if save_extractor
// landed; that resets the consecutive-fails counter.
func (g *healGate) release(success bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.running = false
	if success {
		g.consecutiveFails = 0
		g.lastFailAt = time.Time{}
		return
	}
	g.consecutiveFails++
	g.lastFailAt = time.Now()
}

// status returns a snapshot of the gate state. Useful for the Debug
// page or telemetry. Not part of the critical path.
func (g *healGate) status() GateStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	return GateStatus{
		Running:             g.running,
		ConsecutiveFails:    g.consecutiveFails,
		LastFailAt:          g.lastFailAt,
		MaxConsecutiveFails: g.maxConsecutiveFails,
		CooldownS:           int(g.cooldown.Seconds()),
	}
}

// GateStatus is the public snapshot shape.
type GateStatus struct {
	Running             bool      `json:"running"`
	ConsecutiveFails    int       `json:"consecutive_fails"`
	LastFailAt          time.Time `json:"last_fail_at,omitempty"`
	MaxConsecutiveFails int       `json:"max_consecutive_fails"`
	CooldownS           int       `json:"cooldown_s"`
}

// Status returns the global gate's current state.
func Status() GateStatus { return gate.status() }
