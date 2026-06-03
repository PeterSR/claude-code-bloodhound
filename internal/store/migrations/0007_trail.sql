-- Trail: passive per-session work summaries for recently-active sessions.
-- All opt-in; tables stay empty unless trail_enabled. No historical
-- backfill — on first sight of a session we record a watermark and
-- analyse nothing; later cycles analyse only the delta past it.

-- Per-session watermark. Exists BEFORE a brief does: "seen, not yet
-- summarised" is a real state (first sight). analyzed_through_unix_ms is
-- the timestamp of the last session record we've folded into the brief.
CREATE TABLE trail_watermarks (
    session_uuid             TEXT    PRIMARY KEY,
    first_seen_unix_ms       INTEGER NOT NULL,
    analyzed_through_unix_ms INTEGER NOT NULL
);

-- One CURRENT brief per session (upsert). The live "what's in flight".
-- headline is a short (<=60 char) title for dense multi-session lists;
-- summary is the 1-3 sentence body.
CREATE TABLE trail_briefs (
    session_uuid    TEXT    PRIMARY KEY,
    project         TEXT    NOT NULL,
    cwd             TEXT    NOT NULL DEFAULT '',
    headline        TEXT    NOT NULL DEFAULT '',
    summary         TEXT    NOT NULL,
    updated_unix_ms INTEGER NOT NULL,
    analyzed_runs   INTEGER NOT NULL DEFAULT 1
);

-- Normalized session<->repo edges. THE load-bearing table for "what
-- sessions affect what repos" — both directions are one indexed lookup.
--   role:   'primary' (the session's cwd/home repo) | 'incidental'
--           (a repo it reached into to make a change)
--   branch: git branch/worktree in that repo, when the analyzer knows it
CREATE TABLE trail_brief_repos (
    session_uuid TEXT NOT NULL,
    repo         TEXT NOT NULL,
    role         TEXT NOT NULL DEFAULT 'primary',
    branch       TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (session_uuid, repo)
);
CREATE INDEX idx_trail_brief_repos_repo ON trail_brief_repos (repo);

-- Open loops per session. Identity is loop_key, which the analyzer
-- assigns and carries forward across runs (we feed it the prior brief),
-- so a loop's age + status transitions survive rewording.
--   status:      analyzer's view: active|blocked|waiting|done|stale
--   user_status: manual cross-off, STICKY over the analyzer:
--                NULL | 'done' | 'dismissed'. Effective status is
--                COALESCE(user_status, status). A loop with user_status
--                set is immune to auto-stale and analyzer status changes.
--   related_repo: the repo a waiting/blocked loop concerns, so the
--                by-repo view can show cross-session dependencies.
CREATE TABLE trail_open_loops (
    session_uuid         TEXT    NOT NULL,
    loop_key             TEXT    NOT NULL,
    text                 TEXT    NOT NULL,
    status               TEXT    NOT NULL,
    related_repo         TEXT    NOT NULL DEFAULT '',
    first_seen_unix_ms   INTEGER NOT NULL,
    last_seen_unix_ms    INTEGER NOT NULL,
    user_status          TEXT,
    user_note            TEXT    NOT NULL DEFAULT '',
    user_updated_unix_ms INTEGER,
    PRIMARY KEY (session_uuid, loop_key)
);
CREATE INDEX idx_trail_open_loops_status ON trail_open_loops (status);
CREATE INDEX idx_trail_open_loops_related_repo ON trail_open_loops (related_repo);

-- One row per analyzer invocation. The analyzer is its own claude
-- session (--session-id = trail_session_uuid). Recording it is what lets
-- us BOTH exclude it from every user-facing stat AND sum it for cost.
-- Headless mode fills the cost_* columns directly from claude -p's JSON
-- envelope; interactive leaves them 0 and attributes cost via ingest of
-- the trail_session_uuid's own turns.
CREATE TABLE trail_runs (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    trail_session_uuid  TEXT    NOT NULL,
    target_session_uuid TEXT    NOT NULL,
    mode                TEXT    NOT NULL,
    started_unix_ms     INTEGER NOT NULL,
    finished_unix_ms    INTEGER,
    ok                  INTEGER NOT NULL DEFAULT 0,
    error               TEXT    NOT NULL DEFAULT '',
    cost_usd            REAL    NOT NULL DEFAULT 0,
    input_tokens        INTEGER NOT NULL DEFAULT 0,
    output_tokens       INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens   INTEGER NOT NULL DEFAULT 0,
    cache_create_tokens INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_trail_runs_trail_session  ON trail_runs (trail_session_uuid);
CREATE INDEX idx_trail_runs_target_session ON trail_runs (target_session_uuid);

-- The exclusion mechanism. Every user-facing read that should ignore the
-- analyzer's own sessions reads from this view instead of `turns`
-- directly, so trail turns never enter your stats / calibration. When
-- Trail is off, trail_runs is empty and user_turns == turns exactly.
CREATE VIEW user_turns AS
    SELECT * FROM turns
    WHERE session_uuid NOT IN (SELECT trail_session_uuid FROM trail_runs);
