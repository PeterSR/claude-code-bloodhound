package usage

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Modes for Collect, matching the usage_source config key.
const (
	ModeAuto = "auto" // api, falling back to the pty
	ModeAPI  = "api"  // api only
	ModePTY  = "pty"  // pty only
)

// ValidMode reports whether m is a usage_source value Collect understands.
// Empty counts as ModeAuto.
func ValidMode(m string) bool {
	switch m {
	case "", ModeAuto, ModeAPI, ModePTY:
		return true
	}
	return false
}

// CollectOptions configures Collect.
type CollectOptions struct {
	Mode string // "" => ModeAuto
	API  APIOptions
	PTY  Options
	// Gate remembers rate limiting across calls. Optional: a one-shot caller
	// has nothing to remember, the daemon passes one it keeps.
	Gate *APIGate

	// Seams for tests. nil => FetchAPI / Fetch.
	fetchAPI func(context.Context, APIOptions) (Result, error)
	fetchPTY func(context.Context, Options) (Result, error)
}

// Collect reads one account's usage from the source Mode picks. In ModeAuto
// it asks the api first and drives the pty only when the api cannot answer.
//
// fallback is why the api did not answer when the pty did, so the caller can
// log it; it is nil when the api answered or was never asked. err is the
// error of the source that produced res, as with Fetch and FetchAPI.
func Collect(ctx context.Context, opts CollectOptions) (res Result, fallback error, err error) {
	fetchAPI, fetchPTY := opts.fetchAPI, opts.fetchPTY
	if fetchAPI == nil {
		fetchAPI = FetchAPI
	}
	if fetchPTY == nil {
		fetchPTY = Fetch
	}

	switch opts.Mode {
	case ModePTY:
		res, err = fetchPTY(ctx, opts.PTY)
		return res, nil, err
	case ModeAPI:
		res, err = fetchAPI(ctx, opts.API)
		opts.Gate.note(opts.API.ConfigDir, err)
		return res, nil, err
	case "", ModeAuto:
	default:
		return Result{}, nil, fmt.Errorf("unknown usage source %q (want auto, api or pty)", opts.Mode)
	}

	if until, blocked := opts.Gate.blocked(opts.API.ConfigDir); blocked {
		fallback = fmt.Errorf("%w until %s", ErrAPIBackoff, until.Local().Format("15:04:05"))
	} else {
		res, err = fetchAPI(ctx, opts.API)
		opts.Gate.note(opts.API.ConfigDir, err)
		if err == nil {
			return res, nil, nil
		}
		fallback = err
	}
	// The caller's own deadline or shutdown is not the api's fault, and
	// starting a 10s pty on a cancelled context would only fail slower.
	if ctx.Err() != nil {
		return res, nil, ctx.Err()
	}
	res, err = fetchPTY(ctx, opts.PTY)
	return res, fallback, err
}

// ErrAPIBackoff is the fallback reason while a dir's api is backed off after
// a rate limit.
var ErrAPIBackoff = errors.New("usage endpoint backed off after a rate limit")

// RateLimitBackoff is the least time the api is left alone after a 429.
//
// The endpoint's budget is roughly thirty requests per trailing hour, and
// capacity only returns as old requests age out (claude-swap measured this
// in detail). A 429 often comes with Retry-After: 0, which says "the window
// is saturated" rather than "try now". Something else is spending that
// budget, often Claude Code itself on a busy account, so asking again on
// every five-minute poll would only add to it. The pty answers meanwhile.
const RateLimitBackoff = 15 * time.Minute

// APIGate holds per-dir api backoff across Collect calls. The zero value is
// not usable; a nil *APIGate never blocks and remembers nothing.
type APIGate struct {
	mu    sync.Mutex
	until map[string]time.Time
	now   func() time.Time
}

// NewAPIGate returns an empty gate.
func NewAPIGate() *APIGate {
	return &APIGate{until: map[string]time.Time{}, now: time.Now}
}

func (g *APIGate) blocked(dir string) (time.Time, bool) {
	if g == nil {
		return time.Time{}, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	u, ok := g.until[dir]
	if !ok {
		return time.Time{}, false
	}
	if !g.now().Before(u) {
		delete(g.until, dir)
		return time.Time{}, false
	}
	return u, true
}

// note records the outcome of an api attempt. Only a rate limit sets a
// backoff: every other failure is either cheap to retry or fixed by the pty
// run that follows it (an expired token is refreshed by claude starting up).
func (g *APIGate) note(dir string, err error) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusTooManyRequests {
		if err == nil {
			delete(g.until, dir)
		}
		return
	}
	g.until[dir] = g.now().Add(max(ae.RetryAfter, RateLimitBackoff))
}
