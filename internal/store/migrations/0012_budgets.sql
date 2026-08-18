-- Budgets: what one working directory is allowed to spend, and the leases
-- keeping that allowance in force.
--
-- Absorbed from claude-usage-governor, which kept these in a JSON file beside
-- its own config and read bloodhound's numbers back out of the CLI. Here they
-- are rows next to the attribution that measures them, so evaluating a budget
-- is a query rather than a subprocess.
--
-- Two things changed shape in the move and both are deliberate. The governor
-- spelled its buckets "5h" and "week"; bloodhound has always spelled the same
-- two "session" and "week", and that spelling wins. And a budget is keyed by
-- working directory, never by project: this repo already measured 140 distinct
-- directories collapsing into 44 project names, one project spanning up to ten
-- directories, so a project-keyed budget would silently govern work the user
-- never meant to include.

-- The event log gains a cwd scope. Budget pressure is per-directory, and
-- without this a budget level could only be recorded against a bucket or a
-- session, neither of which is what it is about. Existing rows get '', which
-- reads as "not directory-scoped" exactly as it does for bucket and session.
ALTER TABLE events ADD COLUMN cwd TEXT NOT NULL DEFAULT '';

-- event_levels needs cwd in its primary key, not just as a column: two
-- directories can hold the same kind in different states at the same time, and
-- the old key would collapse them onto one row. SQLite cannot alter a primary
-- key in place, so the table is rebuilt and its contents carried across.
CREATE TABLE event_levels_new (
  kind         TEXT    NOT NULL,
  bucket       TEXT    NOT NULL DEFAULT '',
  session_uuid TEXT    NOT NULL DEFAULT '',
  cwd          TEXT    NOT NULL DEFAULT '',
  state        TEXT    NOT NULL,
  since_ms     INTEGER NOT NULL,
  PRIMARY KEY (kind, bucket, session_uuid, cwd)
);

INSERT INTO event_levels_new (kind, bucket, session_uuid, cwd, state, since_ms)
  SELECT kind, bucket, session_uuid, '', state, since_ms FROM event_levels;

DROP TABLE event_levels;
ALTER TABLE event_levels_new RENAME TO event_levels;

CREATE INDEX idx_events_cwd ON events(cwd);

-- One directory's allowance against one limit bucket.
--
-- The two rules differ in which number they watch, and a budget may carry
-- either or both. spend_pct is measured by attribution: how much of the
-- meter's movement this directory itself caused, so a concurrent project does
-- not consume it and pausing does not either. meter_pct watches the shared
-- meter whoever moved it, which is what buys headroom at the top for work
-- nobody governed. Zero means the rule is not set; both zero is not a budget
-- and is refused before it reaches here.
CREATE TABLE budgets (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  cwd         TEXT    NOT NULL,
  bucket      TEXT    NOT NULL,               -- "session" | "week"
  spend_pct   REAL    NOT NULL DEFAULT 0,
  meter_pct   REAL    NOT NULL DEFAULT 0,
  note        TEXT    NOT NULL DEFAULT '',
  set_ms      INTEGER NOT NULL,
  -- Set when the last lease ran out or the user revoked it. A retired budget
  -- is kept rather than deleted so `budget list --all` can say what happened,
  -- and so re-setting one in the same directory reads as a new row rather
  -- than an edit of history.
  retired_ms  INTEGER,
  retired_why TEXT    NOT NULL DEFAULT ''     -- "revoked" | "expired" | ""
);

-- At most one live budget per (cwd, bucket). Retired rows are exempt, which is
-- what makes the archive possible at all.
CREATE UNIQUE INDEX idx_budgets_live ON budgets(cwd, bucket) WHERE retired_ms IS NULL;
CREATE INDEX idx_budgets_cwd ON budgets(cwd);

-- The leases keeping a budget alive.
--
-- The polarity is inverted from the obvious one and that inversion is load
-- bearing: a lease is not a rule that kills a budget, it is one that keeps it
-- alive. A budget is in force while at least one lease still holds and retires
-- when the last runs out. So a perpetual budget is not one with no leases, it
-- is one holding a lease that never expires, and an empty lease set can only
-- ever mean retired. Liveness is then structural rather than a status field
-- somebody has to remember to update.
--
-- A lease carries everything needed to evaluate it rather than reading it off
-- the budget, so adding one cannot change what another already means.
CREATE TABLE budget_leases (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  budget_id       INTEGER NOT NULL REFERENCES budgets(id) ON DELETE CASCADE,
  kind            TEXT    NOT NULL,           -- "manual" | "deadline" | "window_reset"
  -- deadline: when it runs out.
  at_ms           INTEGER,
  -- window_reset: which bucket's window, when this lease was bound, and the
  -- end of the window that was open at that moment. window_end_ms is null when
  -- no window was open then, because the session window is usage-triggered and
  -- the next one begins whenever work next begins. Storing the end when it is
  -- already known keeps the common case pure clock arithmetic.
  bucket          TEXT    NOT NULL DEFAULT '',
  bound_after_ms  INTEGER,
  window_end_ms   INTEGER,
  expired_ms      INTEGER
);

CREATE INDEX idx_budget_leases_budget ON budget_leases(budget_id);
