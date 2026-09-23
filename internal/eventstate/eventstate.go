// Package eventstate builds the event-log read payload that both
// `bloodhound events` and GET /api/events serve.
//
// Same reason internal/nowstate exists: the CLI reads SQLite directly and the
// handler goes through the socket, and the two must not drift on a cursor or a
// freshness figure a consumer might compare across them.
//
// It sits above internal/events because it needs nowstate (and therefore
// store) for the freshness envelope, which internal/events deliberately cannot
// reach.
package eventstate

import (
	"context"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
	"github.com/PeterSR/claude-code-bloodhound/internal/events"
	"github.com/PeterSR/claude-code-bloodhound/internal/nowstate"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

// Response is one read of the log.
//
// Freshness travels with the events rather than being a separate call. An
// empty Events from a database nobody has written to in an hour means
// something very different from an empty Events from a healthy one, and a
// consumer must not be able to act on the first while believing the second.
type Response struct {
	OK          bool            `json:"ok"`
	NowMS       int64           `json:"server_now_ms"`
	Cursor      Cursor          `json:"cursor"`
	LastPoll    *routes.NowPoll `json:"last_poll"`
	StaleAfterS int             `json:"stale_after_s,omitempty"`
	Stale       bool            `json:"stale"`
	Levels      []events.Level  `json:"levels,omitempty"`
	Events      []events.Event  `json:"events,omitempty"`
}

// Cursor is where this read started and ended. More is set when the page was
// truncated, so a consumer knows to come straight back rather than wait.
type Cursor struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
	More bool  `json:"more"`
}

// Compute reads the log and wraps it in the freshness envelope.
func Compute(ctx context.Context, s *store.Store, now time.Time, f events.Filter, withLevels bool) (*Response, error) {
	out := &Response{OK: true, NowMS: now.UnixMilli()}

	// Deliberately non-fatal: a log read that cannot describe its own
	// freshness is still worth returning, as long as the missing LastPoll says
	// so rather than implying health.
	// Freshness is of the meter the filter asks about, or the primary one.
	acct := f.Account
	if acct == 0 {
		acct, _ = s.PrimaryAccountID(ctx)
	}
	if pool, err := nowstate.Compute(ctx, s, acct, now); err == nil {
		out.LastPoll = pool.LastPoll
		out.StaleAfterS = pool.StaleAfterS
		if pool.LastPoll != nil && pool.StaleAfterS > 0 {
			out.Stale = pool.LastPoll.AgeS > int64(pool.StaleAfterS)
		}
	}

	if withLevels {
		lv, err := events.Levels(ctx, s.DB)
		if err != nil {
			return nil, err
		}
		out.Levels = lv
	}

	evs, err := events.Query(ctx, s.DB, f)
	if err != nil {
		return nil, err
	}
	out.Events = evs
	out.Cursor.From = f.SinceID
	out.Cursor.To = f.SinceID
	if n := len(evs); n > 0 {
		out.Cursor.To = evs[n-1].ID
		limit := f.Limit
		if limit <= 0 {
			limit = events.DefaultLimit
		}
		out.Cursor.More = n >= limit
	}
	return out, nil
}
