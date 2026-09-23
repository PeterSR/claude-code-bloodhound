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

	// One pool per metered account, primary first. A failure here is not
	// fatal. The collection sensor's whole job is to report that the meter is
	// unreadable, and it cannot do that if an unreadable meter stops the pass.
	ids, err := s.MeteredAccountIDs(ctx)
	if err == nil {
		for i, id := range ids {
			pool, perr := nowstate.Compute(ctx, s, id, now)
			if perr != nil {
				err = perr
				continue
			}
			if i == 0 {
				w.Pool = pool
			}
			w.Meters = append(w.Meters, events.AccountPool{Account: id, Pool: pool})
		}
	}
	if len(ids) > 0 {
		w.TokensPerPctCW, _, _, w.HasCalibration, _ = s.LatestCalibrationMedian(ctx, ids[0], "session", 10)
	}

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

// meterWindow is one bucket of one account's meter.
type meterWindow struct {
	bucket  string
	win     *routes.NowWindow
	account int64
}

// meterWindows lists every bucket of every metered account. An account whose
// pool failed to compute is absent from Meters and so contributes nothing,
// which is the nil-pool bail the comment on buckets asks for, per account.
func meterWindows(w events.World) []meterWindow {
	var out []meterWindow
	for _, m := range w.Meters {
		for bucket, win := range buckets(m.Pool) {
			out = append(out, meterWindow{bucket, win, m.Account})
		}
	}
	return out
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
			var out []events.Reading
			for _, mw := range meterWindows(w) {
				win := mw.win
				r := events.Reading{Kind: "limit_projection", Scope: events.Scope{Bucket: mw.bucket, Account: mw.account}}
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
				withReset(&r, win)
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
			var out []events.Reading
			for _, mw := range meterWindows(w) {
				win := mw.win
				r := events.Reading{Kind: "threshold", Scope: events.Scope{Bucket: mw.bucket, Account: mw.account}}
				if win == nil {
					r.State = "" // recorded as unknown, never silently dropped
				} else {
					r.State = fmt.Sprintf("%d", (win.Pct/10)*10)
					r.Detail = map[string]any{"pct": win.Pct}
					withReset(&r, win)
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
			var out []events.Reading
			for _, mw := range meterWindows(w) {
				win := mw.win
				r := events.Reading{Kind: "saturation", Scope: events.Scope{Bucket: mw.bucket, Account: mw.account}}
				switch {
				case win == nil:
					r.State = ""
				case win.Saturated:
					r.State = "saturated"
					r.Detail = map[string]any{"pct": win.Pct}
				default:
					r.State = "clear"
				}
				withReset(&r, win)
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
			// An account whose pool would not compute is absent from Meters,
			// which says nothing about collection health. Staying silent
			// leaves its recorded level alone rather than laundering a
			// transient database error into a claim about the meter.
			var out []events.Reading
			for _, m := range w.Meters {
				pool := m.Pool
				r := events.Reading{Kind: "collection", Scope: events.Scope{Account: m.Account}}
				switch {
				case pool.LastPoll == nil:
					r.State = "" // nothing captured yet, so we assert nothing
				case !pool.LastPoll.ParseOK:
					// The known drift mode: the panel rendered but the extractor
					// missed. Self-heal usually catches it on the next poll.
					r.State = "extraction_failed"
					r.Detail = map[string]any{"age_s": pool.LastPoll.AgeS, "ts": pool.LastPoll.TSISO}
				case pool.StaleAfterS > 0 && pool.LastPoll.AgeS > int64(pool.StaleAfterS):
					r.State = "stale"
					r.Detail = map[string]any{
						"age_s":         pool.LastPoll.AgeS,
						"stale_after_s": pool.StaleAfterS,
					}
				default:
					r.State = "ok"
					r.Detail = map[string]any{"age_s": pool.LastPoll.AgeS}
				}
				out = append(out, r)
			}
			return out, nil
		},
	})
}

// withReset records when the window a reading is about turns over.
//
// Every meter reading is about pressure inside a window that ends, and the
// end is half the answer: "the 5h meter will hit the cap" is a different
// message depending on whether the window reopens in twenty minutes or on
// Friday. Announcements read from the event rather than from live state, so
// the moment has to be captured at the time the reading was taken; recomputing
// it at delivery would be answering about a window that may already have
// turned over.
//
// Absolute rather than a countdown, for the same reason. An event sits in the
// log and is read later, and a stored duration silently ages.
func withReset(r *events.Reading, win *routes.NowWindow) {
	if win == nil || win.ResetTSISO == "" || r.State == "" {
		return
	}
	if r.Detail == nil {
		r.Detail = map[string]any{}
	}
	r.Detail["reset_ts"] = win.ResetTSISO
}
