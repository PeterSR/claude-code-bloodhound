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

-- Normalized session<->worktree edges. THE load-bearing table for "what
-- sessions affect what repos" — both directions are one indexed lookup.
-- Identity is the ABSOLUTE worktree path: from it we derive the display
-- dirname (basename) and the branch LIVE (git -C path branch
-- --show-current) at render time, so they never go stale. We assume
-- worktrees don't move; if one is gone, branch_cached is the fallback.
--   role:         'primary' (the session's cwd/home worktree) |
--                 'incidental' (a worktree it reached into)
--   branch_cached: last-known branch, written each run; live wins on read
CREATE TABLE trail_brief_repos (
    session_uuid  TEXT NOT NULL,
    repo_path     TEXT NOT NULL,
    role          TEXT NOT NULL DEFAULT 'primary',
    branch_cached TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (session_uuid, repo_path)
);
CREATE INDEX idx_trail_brief_repos_path ON trail_brief_repos (repo_path);

-- Open loops per session. Identity is loop_key, which the analyzer
-- assigns and carries forward across runs (we feed it the prior brief),
-- so a loop's age + status transitions survive rewording.
--   status:      analyzer's view: active|blocked|waiting|done|stale
--   user_status: manual cross-off, STICKY over the analyzer:
--                NULL | 'done' | 'dismissed'. Effective status is
--                COALESCE(user_status, status). A loop with user_status
--                set is immune to auto-stale and analyzer status changes.
--   related_repo_path: the worktree path a waiting/blocked loop
--                concerns, so the by-repo view can show cross-session
--                dependencies (joins to trail_brief_repos.repo_path).
CREATE TABLE trail_open_loops (
    session_uuid         TEXT    NOT NULL,
    loop_key             TEXT    NOT NULL,
    text                 TEXT    NOT NULL,
    status               TEXT    NOT NULL,
    related_repo_path    TEXT    NOT NULL DEFAULT '',
    first_seen_unix_ms   INTEGER NOT NULL,
    last_seen_unix_ms    INTEGER NOT NULL,
    user_status          TEXT,
    user_note            TEXT    NOT NULL DEFAULT '',
    user_updated_unix_ms INTEGER,
    PRIMARY KEY (session_uuid, loop_key)
);
CREATE INDEX idx_trail_open_loops_status ON trail_open_loops (status);
CREATE INDEX idx_trail_open_loops_related_repo ON trail_open_loops (related_repo_path);

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

-- Exclusion of the analyzer's own sessions is done UPSTREAM, in the
-- ingester: it skips any JSONL file whose session UUID is in trail_runs,
-- so analyzer turns never enter `turns` at all — no downstream query
-- needs to filter them, and they can't pollute stats / calibration.
-- (trail_runs is written at analyzer launch, before its JSONL exists, so
-- the skip is reliable.) Cost is attributed by summing the analyzer's
-- tokens into trail_runs.cost_* rather than via the turns table.
