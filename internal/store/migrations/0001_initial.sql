-- 0001_initial.sql — full v1 schema in one migration.
--
-- Pre-1.0 we'd rather wipe the DB than carry incremental migrations for an
-- unstable schema. Once we ship a tagged release this file freezes and any
-- further changes land as 0002_*.sql, 0003_*.sql, etc.

CREATE TABLE meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- One row per assistant turn in a session JSONL file.
-- (session_uuid, turn_idx) is the natural key. We do NOT persist user/action
-- text here; that stays in the JSONL and is loaded on demand by the per-session
-- view to keep this table small and free of conversation content.
CREATE TABLE turns (
    session_uuid     TEXT    NOT NULL,
    turn_idx         INTEGER NOT NULL,
    ts               TEXT    NOT NULL,        -- ISO-8601 UTC
    ts_unix_ms       INTEGER NOT NULL,        -- denormalized for range queries
    model            TEXT    NOT NULL,
    input_tokens     INTEGER NOT NULL,
    output_tokens    INTEGER NOT NULL,
    cache_read       INTEGER NOT NULL,
    cache_create_5m  INTEGER NOT NULL,
    cache_create_1h  INTEGER NOT NULL,
    gap_s            REAL    NOT NULL,        -- seconds since previous turn in this session
    classification   TEXT    NOT NULL,        -- normal | idle_miss | rotation | restructure
    post_compact     INTEGER NOT NULL DEFAULT 0,
    project          TEXT    NOT NULL,        -- sanitized project dir name
    source_path_hash TEXT    NOT NULL,        -- sha256 of source jsonl path (no PII leak from path)
    PRIMARY KEY (session_uuid, turn_idx)
);

CREATE INDEX idx_turns_ts_unix_ms ON turns (ts_unix_ms);
CREATE INDEX idx_turns_session ON turns (session_uuid);
CREATE INDEX idx_turns_project ON turns (project);

-- One row per detected /compact event. confirmed=1 only when all three
-- detection signals agreed (compact_boundary + isCompactSummary + prefix shrink).
CREATE TABLE compactions (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    session_uuid        TEXT    NOT NULL,
    ts                  TEXT    NOT NULL,
    ts_unix_ms          INTEGER NOT NULL,
    prefix_tokens_est   INTEGER NOT NULL,
    summary_tokens_est  INTEGER NOT NULL,
    gap_to_prev_s       REAL,                 -- nullable: unknown if compaction at session start
    cache_state         TEXT    NOT NULL,     -- cold | warm_1h | warm_5m | unknown
    confirmed           INTEGER NOT NULL,
    confirm_reason      TEXT    NOT NULL DEFAULT '',
    project             TEXT    NOT NULL
);

CREATE INDEX idx_compactions_ts_unix_ms ON compactions (ts_unix_ms);
CREATE INDEX idx_compactions_session ON compactions (session_uuid);

-- One row per /usage scrape.
CREATE TABLE usage_observations (
    id                       INTEGER PRIMARY KEY AUTOINCREMENT,
    ts                       TEXT    NOT NULL,
    ts_unix_ms               INTEGER NOT NULL,
    session_pct              INTEGER,
    week_pct                 INTEGER,
    session_reset_raw        TEXT,             -- raw string from /usage panel ("May 1, 1am")
    week_reset_raw           TEXT,
    session_reset_ts         TEXT,             -- parsed ISO-8601 UTC; NULL if parse failed
    week_reset_ts            TEXT,
    raw_dump_id              INTEGER,
    session_reset_detected   INTEGER NOT NULL DEFAULT 0,
    week_reset_detected      INTEGER NOT NULL DEFAULT 0,
    elapsed_s                REAL,             -- how long the scrape took
    parse_ok                 INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (raw_dump_id) REFERENCES raw_dumps (id) ON DELETE SET NULL
);

CREATE INDEX idx_usage_obs_ts_unix_ms ON usage_observations (ts_unix_ms);

-- Cleaned-and-stripped TUI dump corresponding to a usage_observation.
-- Capped + rotated by the aggregator so it doesn't grow without bound.
CREATE TABLE raw_dumps (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    ts_unix_ms  INTEGER NOT NULL,
    payload     TEXT    NOT NULL
);

-- Denormalized session summary, refreshed by the aggregator. Lets the
-- Sessions list page render fast without scanning all turns.
CREATE TABLE sessions (
    session_uuid           TEXT    PRIMARY KEY,
    project                TEXT    NOT NULL,
    first_ts_unix_ms       INTEGER NOT NULL,
    last_ts_unix_ms        INTEGER NOT NULL,
    turn_count             INTEGER NOT NULL,
    raw_tokens             INTEGER NOT NULL,
    output_tokens          INTEGER NOT NULL,
    peak_5h_raw_tokens     INTEGER NOT NULL,
    idle_miss_count        INTEGER NOT NULL,
    rotation_count         INTEGER NOT NULL,
    restructure_count      INTEGER NOT NULL,
    compaction_count       INTEGER NOT NULL,
    cold_compaction_count  INTEGER NOT NULL,
    cache_ttl              TEXT    NOT NULL,    -- 1h | 5m | mix | none
    models                 TEXT    NOT NULL     -- comma-separated
);

CREATE INDEX idx_sessions_last_ts ON sessions (last_ts_unix_ms);
CREATE INDEX idx_sessions_project ON sessions (project);

-- Derived 5-hour windows. reset_ts_unix_ms anchors the right edge of each
-- bucket; when it comes from /usage's parsed reset string it's authoritative,
-- otherwise it's inferred and inferred=1.
CREATE TABLE buckets (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    start_unix_ms         INTEGER NOT NULL,
    end_unix_ms           INTEGER NOT NULL,
    reset_inferred        INTEGER NOT NULL,    -- 1 = inferred; 0 = anchored to /usage reset_ts
    raw_token_total       INTEGER NOT NULL,
    cost_weighted_total   REAL    NOT NULL,
    output_token_total    INTEGER NOT NULL,
    turn_count            INTEGER NOT NULL,
    UNIQUE (start_unix_ms)
);

CREATE INDEX idx_buckets_end ON buckets (end_unix_ms);

-- Tracks which JSONL files we've already ingested and their mtimes.
-- Skip files whose mtime hasn't changed.
CREATE TABLE ingested_files (
    path_hash        TEXT    PRIMARY KEY,
    path             TEXT    NOT NULL,
    mtime_unix       INTEGER NOT NULL,
    last_ingested_ts TEXT    NOT NULL,
    turn_count       INTEGER NOT NULL DEFAULT 0,
    compaction_count INTEGER NOT NULL DEFAULT 0
);
