-- Events: an append-only log of state transitions, the levels that produce
-- them, and one-shot hooks that fire on a match.
--
-- Design notes live in .agent-workspace/PLAN_EVENTS.md. The short version:
-- edges (a reset, a self-heal) are appended directly by whatever transaction
-- discovered them; levels (a projected limit, a saturation, a pct band) are
-- reported by pure sensors and diffed by the reconciler, which owns the memory
-- so read-side computations like nowstate.fillBurn can stay stateless.

CREATE TABLE events (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,  -- the cursor
  ts_unix_ms     INTEGER NOT NULL,
  kind           TEXT    NOT NULL,                   -- "limit_projection.projected"
  bucket         TEXT    NOT NULL DEFAULT '',        -- "session" | "week" | ''
  session_uuid   TEXT    NOT NULL DEFAULT '',
  project        TEXT    NOT NULL DEFAULT '',
  prev_state     TEXT    NOT NULL DEFAULT '',        -- what it came from; '' for edges
  detail         TEXT    NOT NULL DEFAULT ''         -- JSON object, kind-specific
);

CREATE INDEX idx_events_ts ON events(ts_unix_ms);
CREATE INDEX idx_events_kind ON events(kind);

-- The current value of every level. Doubles as the answer to "what is true
-- now" for a consumer with no cursor, without replaying the log.
CREATE TABLE event_levels (
  kind         TEXT    NOT NULL,
  bucket       TEXT    NOT NULL DEFAULT '',
  session_uuid TEXT    NOT NULL DEFAULT '',
  state        TEXT    NOT NULL,
  since_ms     INTEGER NOT NULL,
  PRIMARY KEY (kind, bucket, session_uuid)
);

-- One-shot hooks. Registered imperatively by a live process, fire once, and
-- are then done. Rows are kept after firing so `bloodhound when --list` can
-- show what happened.
CREATE TABLE event_hooks (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  created_ms     INTEGER NOT NULL,
  after_event_id INTEGER NOT NULL,                   -- "next" means strictly after this
  kind_glob      TEXT    NOT NULL,
  bucket         TEXT    NOT NULL DEFAULT '',
  session_uuid   TEXT    NOT NULL DEFAULT '',
  command        TEXT    NOT NULL,
  note           TEXT    NOT NULL DEFAULT '',
  expires_ms     INTEGER NOT NULL,
  status         TEXT    NOT NULL DEFAULT 'pending', -- pending | fired | expired | cancelled
  fired_ms       INTEGER,
  fired_event_id INTEGER,
  exit_code      INTEGER
);

CREATE INDEX idx_event_hooks_status ON event_hooks(status);
