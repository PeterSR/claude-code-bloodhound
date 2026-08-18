package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/events"
	"github.com/PeterSR/claude-code-bloodhound/internal/eventstate"
)

// Long-poll bounds. maxWait caps how long one request may park; pollEvery is
// how often it re-checks while parked.
const (
	maxWait   = 30 * time.Second
	pollEvery = 2 * time.Second
)

// handleEvents serves the event log with a cursor, optionally parking until
// something lands.
//
//	GET /api/events?since=412&kind=cache.*&bucket=session&wait=25
//
// wait makes this a long-poll rather than a stream. There is no SSE or
// websocket anywhere in this API and no test harness for one; a handler that
// sleeps is an ordinary handler, testable the same way as every other route,
// and needs no client library on the React side. The client loop is
// `since = resp.cursor.to` and call again.
//
// Parking still returns the freshness envelope on timeout, so a quiet channel
// and a dead collector stay distinguishable. That property is the whole reason
// this is safe to build on.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()

	f := events.Filter{
		Kinds:   q["kind"],
		Bucket:  q.Get("bucket"),
		Session: q.Get("session"),
		Cwd:     normalizeCwdQuery(q.Get("cwd")),
	}
	if v := q.Get("since"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"ok": false, "error": "since must be a cursor id",
			})
			return
		}
		f.SinceID = id
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			f.Limit = n
		}
	}

	var wait time.Duration
	if v := q.Get("wait"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			wait = time.Duration(n) * time.Second
			if wait > maxWait {
				wait = maxWait
			}
		}
	}

	deadline := time.Now().Add(wait)
	for {
		out, err := eventstate.Compute(ctx, s.Store, time.Now(), f, q.Get("levels") == "1")
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		// Anything to say, or nothing left to wait for.
		if len(out.Events) > 0 || wait == 0 || !time.Now().Before(deadline) {
			writeJSON(w, http.StatusOK, out)
			return
		}
		select {
		case <-ctx.Done():
			return // client hung up
		case <-time.After(pollEvery):
		}
	}
}

// handleEventsLevels serves what is true right now, with no replay.
//
// This is the shape a short-lived consumer wants: a statusline redrawing every
// second holds no cursor and does not care how the current state was reached.
// Handing it a log would be the wrong tool.
func (s *Server) handleEventsLevels(w http.ResponseWriter, r *http.Request) {
	out, err := eventstate.Compute(r.Context(), s.Store, time.Now(),
		events.Filter{Limit: 1}, true)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	out.Events = nil
	writeJSON(w, http.StatusOK, out)
}
