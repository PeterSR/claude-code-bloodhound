package priceheal

import (
	"errors"
	"sync"
	"time"
)

// The price-heal gate differs from the extractor's on purpose.
//
// The extractor has one artifact, so one global mutex plus one cool-down is
// the whole story. Prices are per model: a model that can't be priced (a
// retired name, a typo, a search that keeps coming up empty) must not be
// retried every poll, but that must never block pricing a different model
// that appears the same day. So the backoff is keyed by model, with a
// single global mutex on top so only one heal drives claude at a time.

var (
	// ErrAlreadyRunning means another heal holds the single slot.
	ErrAlreadyRunning = errors.New("price heal already running")
	// ErrCoolingDown means this model failed recently and is in backoff.
	ErrCoolingDown = errors.New("price heal for this model is in cool-down")
)

type attempt struct {
	fails  int
	lastAt time.Time
}

// Gate serializes heals and backs off per model after failures.
type Gate struct {
	mu       sync.Mutex
	running  bool
	perModel map[string]attempt
	cooldown time.Duration
	maxFails int
	now      func() time.Time // injectable for tests
}

// NewGate builds a gate with the default backoff: after 2 straight failures
// for a model, wait 6 hours before trying it again. Prices change rarely, so
// a slow retry is appropriate; a model that stays unpriced is a flagged,
// graceful state, not an outage.
func NewGate() *Gate {
	return &Gate{
		perModel: map[string]attempt{},
		cooldown: 6 * time.Hour,
		maxFails: 2,
		now:      time.Now,
	}
}

// tryAcquire claims the single heal slot for a model. force bypasses the
// per-model cool-down (a user pressing "find out for me"), but never the
// mutex. The caller must call release exactly once.
func (g *Gate) tryAcquire(model string, force bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.running {
		return ErrAlreadyRunning
	}
	if !force {
		a := g.perModel[model]
		if a.fails >= g.maxFails && g.now().Sub(a.lastAt) < g.cooldown {
			return ErrCoolingDown
		}
	}
	g.running = true
	return nil
}

// release frees the slot. On success the model's backoff is cleared (it is
// now priced and won't be asked for again); on failure the fail count grows.
func (g *Gate) release(model string, success bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.running = false
	if success {
		delete(g.perModel, model)
		return
	}
	a := g.perModel[model]
	a.fails++
	a.lastAt = g.now()
	g.perModel[model] = a
}

// Backoff reports whether a model is currently in cool-down, for the UI.
func (g *Gate) Backoff(model string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	a := g.perModel[model]
	return a.fails >= g.maxFails && g.now().Sub(a.lastAt) < g.cooldown
}
