// Package sensors holds the level sensors and the wiring that runs them.
//
// Importing this package registers every sensor as a side effect, which is how
// a caller turns events on. It sits below internal/events rather than inside
// it because it needs store and nowstate, and internal/events must stay clear
// of both so that store can append edges without an import cycle.
//
// Starting deliberately small. The meter facts here are the ones that are pure
// byproducts of a poll that already happened, so nothing new is measured and
// nothing new is spent. Session-scoped sensors (cache expiry, recommendation
// changes) want a faster tick than the 300s poll and are not wired up yet.
package sensors

import (
	"context"
	"fmt"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
	"github.com/PeterSR/claude-code-bloodhound/internal/events"
	"github.com/PeterSR/claude-code-bloodhound/internal/nowstate"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

// Run computes the shared world once and reconciles. This is the entry point
// for the daemon tick and for `bloodhound events reconcile`.
//
// Attached here rather than in the daemon so a cron-scheduled install behaves
// identically to a daemon-scheduled one: both call the same function, and
// neither needs a socket.
func Run(ctx context.Context, s *store.Store, now time.Time) (events.Stats, error) {
	w := events.World{DB: s.DB, Now: now}

	// A failure here is not fatal. The collection sensor's whole job is to
	// report that the meter is unreadable, and it cannot do that if an
	// unreadable meter stops the pass.
	pool, err := nowstate.Compute(ctx, s, now)
	if err == nil {
		w.Pool = pool
	}
	w.TokensPerPctCW, _, _, w.HasCalibration, _ = s.LatestCalibrationMedian(ctx, "session", 10)

	st, rerr := events.Reconcile(ctx, s.DB, w)
	if err != nil {
		st.Errors = append(st.Errors, fmt.Sprintf("nowstate: %v", err))
	}
	return st, rerr
}

// buckets pairs each limit bucket with its window. A nil window means the pool
// state was computed but that bucket had no reading in it, which is a real
// "we do not know" and distinct from the pool failing to compute at all.
//
// Callers must bail on a nil pool BEFORE calling this. That distinction is
// load-bearing: treating a transient failure to compute as "actively unknown"
// would flip every level to unknown and straight back on the next tick, and a
// one-shot watching `limit_projection.*` would be consumed by the noise.
func buckets(pool *routes.NowResponse) map[string]*routes.NowWindow {
	return map[string]*routes.NowWindow{"session": pool.Session, "week": pool.Week}
}

func init() {
	// Whether the pace of the last hour crosses 100% before this window
	// resets. nowstate.fillBurn already decides this, including the deliberate
	// suppression when the crossing lands after the natural reset; the sensor
	// only reports what it found. fillBurn stays a stateless per-call
	// recomputation, and the reconciler holds the memory.
	events.Register(events.Sensor{
		Name: "burn/limit",
		Read: func(ctx context.Context, w events.World) ([]events.Reading, error) {
			if w.Pool == nil {
				return nil, nil // nothing to say, which is not the same as not knowing
			}
			var out []events.Reading
			for bucket, win := range buckets(w.Pool) {
				r := events.Reading{Kind: "limit_projection", Scope: events.Scope{Bucket: bucket}}
				switch {
				case win == nil:
					r.State = "" // recorded as unknown, never silently dropped
				case win.LimitOK:
					r.State = "projected"
					r.Detail = map[string]any{
						"pct":            win.Pct,
						"eta_ms":         win.LimitETAMS,
						"eta_ts":         win.LimitETATS,
						"burn_pct_per_h": win.BurnPctPerHour,
					}
				default:
					r.State = "clear"
					r.Detail = map[string]any{"pct": win.Pct}
				}
				out = append(out, r)
			}
			return out, nil
		},
	})

	// Coarse pct bands. The band name is the level, so 78 to 82 emits exactly
	// one event and dropping back after a reset emits exactly one the other
	// way, with no bespoke hysteresis anywhere.
	events.Register(events.Sensor{
		Name: "meter/threshold",
		Read: func(ctx context.Context, w events.World) ([]events.Reading, error) {
			if w.Pool == nil {
				return nil, nil
			}
			var out []events.Reading
			for bucket, win := range buckets(w.Pool) {
				r := events.Reading{Kind: "threshold", Scope: events.Scope{Bucket: bucket}}
				if win == nil {
					r.State = "" // recorded as unknown, never silently dropped
				} else {
					r.State = fmt.Sprintf("%d", (win.Pct/10)*10)
					r.Detail = map[string]any{"pct": win.Pct}
				}
				out = append(out, r)
			}
			return out, nil
		},
	})

	// At or above the saturation threshold, meaning pct has stopped moving
	// while spend continues. Every derived figure downstream is an estimate
	// from here on, which is time-sensitive in a way a polled caveat is not.
	events.Register(events.Sensor{
		Name: "meter/saturation",
		Read: func(ctx context.Context, w events.World) ([]events.Reading, error) {
			if w.Pool == nil {
				return nil, nil
			}
			var out []events.Reading
			for bucket, win := range buckets(w.Pool) {
				r := events.Reading{Kind: "saturation", Scope: events.Scope{Bucket: bucket}}
				switch {
				case win == nil:
					r.State = ""
				case win.Saturated:
					r.State = "saturated"
					r.Detail = map[string]any{"pct": win.Pct}
				default:
					r.State = "clear"
				}
				out = append(out, r)
			}
			return out, nil
		},
	})

	// Collection health. This is the one that lets a consumer distinguish a
	// calm meter from a pipeline that stopped an hour ago, which is the whole
	// reason the log is safe to act on.
	events.Register(events.Sensor{
		Name: "collection/health",
		Read: func(ctx context.Context, w events.World) ([]events.Reading, error) {
			if w.Pool == nil {
				// The pool state itself would not compute, which says nothing
				// about collection health. Staying silent leaves the recorded
				// level alone rather than laundering a transient database
				// error into a claim about the meter.
				return nil, nil
			}
			r := events.Reading{Kind: "collection"}
			switch {
			case w.Pool.LastPoll == nil:
				r.State = "" // nothing captured yet, so we assert nothing
			case !w.Pool.LastPoll.ParseOK:
				// The known drift mode: the panel rendered but the extractor
				// missed. Self-heal usually catches it on the next poll.
				r.State = "extraction_failed"
				r.Detail = map[string]any{"age_s": w.Pool.LastPoll.AgeS, "ts": w.Pool.LastPoll.TSISO}
			case w.Pool.StaleAfterS > 0 && w.Pool.LastPoll.AgeS > int64(w.Pool.StaleAfterS):
				r.State = "stale"
				r.Detail = map[string]any{
					"age_s":         w.Pool.LastPoll.AgeS,
					"stale_after_s": w.Pool.StaleAfterS,
				}
			default:
				r.State = "ok"
				r.Detail = map[string]any{"age_s": w.Pool.LastPoll.AgeS}
			}
			return []events.Reading{r}, nil
		},
	})
}
