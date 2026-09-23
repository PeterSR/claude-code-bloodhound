-- Accounts: which Claude account spent a turn or showed a meter reading.
--
-- Until now bloodhound assumed one account: one ~/.claude, one /usage meter.
-- People run several, either as one CLAUDE_CONFIG_DIR per account or by
-- switching with /login inside one dir, and each account has its own quota.
-- Mixing them makes every meter wrong, because another account's turns get
-- credited against a meter they never moved.
--
-- Transcripts carry no account id, so a turn's account is inferred from where
-- its file lives and when it was written: account_logins records which account
-- each config dir was logged in to, and from when. A turn belongs to the
-- newest login of its dir that started at or before the turn. The first login
-- bloodhound sees for a dir starts at 0, so everything already on disk when
-- the dir was first watched goes to whoever was logged in at that moment.
-- Switches take effect from when they are first noticed (next ingest or poll),
-- not from when they happened. That imprecision is accepted by design.

-- One row per (account, organization). The same login in a personal plan and
-- a Team org has two separate quotas, so the pair is the identity. Dirs with
-- no OAuth login (API key, Bedrock, Vertex) get a synthetic account_uuid of
-- 'dir:<path>' so their turns still attribute somewhere.
--
-- email and org_name stay in this local database. They are for the UI.
CREATE TABLE accounts (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    account_uuid    TEXT    NOT NULL,
    org_uuid        TEXT    NOT NULL DEFAULT '',
    email           TEXT    NOT NULL DEFAULT '',
    org_name        TEXT    NOT NULL DEFAULT '',
    rate_limit_tier TEXT    NOT NULL DEFAULT '',
    label           TEXT    NOT NULL DEFAULT '',   -- user-set display name
    first_seen_ms   INTEGER NOT NULL DEFAULT 0,
    last_seen_ms    INTEGER NOT NULL DEFAULT 0,
    UNIQUE (account_uuid, org_uuid)
);

-- Row 1 owns every row collected before this migration. Its identity is blank
-- because SQL cannot read Claude Code's state file; the first time the
-- default config dir is observed, it claims this row instead of inserting a
-- new one. The ('', '') key cannot collide with a real account.
INSERT INTO accounts (id, account_uuid, org_uuid) VALUES (1, '', '');

-- Who was logged in to a config dir from from_ms on, until the next row for
-- the same dir. No end column: the next row is the end.
CREATE TABLE account_logins (
    config_dir TEXT    NOT NULL,
    from_ms    INTEGER NOT NULL,
    account_id INTEGER NOT NULL REFERENCES accounts (id),
    PRIMARY KEY (config_dir, from_ms)
);

-- Every source table gets the account that produced it. Existing rows all
-- came from the single dir bloodhound watched before this, so they belong to
-- row 1. Default 1 is that backfill.
ALTER TABLE turns              ADD COLUMN account_id INTEGER NOT NULL DEFAULT 1;
ALTER TABLE sessions           ADD COLUMN account_id INTEGER NOT NULL DEFAULT 1;
ALTER TABLE compactions        ADD COLUMN account_id INTEGER NOT NULL DEFAULT 1;
ALTER TABLE user_prompts       ADD COLUMN account_id INTEGER NOT NULL DEFAULT 1;
ALTER TABLE quota_signals      ADD COLUMN account_id INTEGER NOT NULL DEFAULT 1;
ALTER TABLE usage_observations ADD COLUMN account_id INTEGER NOT NULL DEFAULT 1;

CREATE INDEX idx_turns_account              ON turns (account_id, ts_unix_ms);
CREATE INDEX idx_usage_obs_account_ts       ON usage_observations (account_id, ts_unix_ms);
CREATE INDEX idx_sessions_account           ON sessions (account_id);
-- Budgets and other directory-scoped views ask which account a cwd last
-- spent on.
CREATE INDEX idx_turns_cwd_ts               ON turns (cwd, ts_unix_ms);

-- The derived meter tables become per account: each account is its own
-- meter, with its own windows, calibration and attribution. They are wiped
-- and rebuilt on every aggregate, so they are recreated empty here with
-- account_id in their keys rather than altered; the next aggregate refills
-- them. Nothing in them is a source of truth.
DROP TABLE session_attribution;
DROP TABLE limit_windows;
DROP TABLE calibration_points;
DROP TABLE buckets;

CREATE TABLE buckets (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id            INTEGER NOT NULL DEFAULT 1,
    start_unix_ms         INTEGER NOT NULL,
    end_unix_ms           INTEGER NOT NULL,
    reset_inferred        INTEGER NOT NULL,
    raw_token_total       INTEGER NOT NULL,
    cost_weighted_total   REAL    NOT NULL,
    output_token_total    INTEGER NOT NULL,
    turn_count            INTEGER NOT NULL,
    UNIQUE (account_id, start_unix_ms)
);
CREATE INDEX idx_buckets_end ON buckets (account_id, end_unix_ms);

CREATE TABLE calibration_points (
    id                       INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id               INTEGER NOT NULL DEFAULT 1,
    bucket                   TEXT    NOT NULL,
    a_obs_id                 INTEGER NOT NULL,
    b_obs_id                 INTEGER NOT NULL,
    a_ts_unix_ms             INTEGER NOT NULL,
    b_ts_unix_ms             INTEGER NOT NULL,
    a_pct                    INTEGER NOT NULL,
    b_pct                    INTEGER NOT NULL,
    delta_pct                INTEGER NOT NULL,
    raw_tokens               INTEGER NOT NULL,
    cost_weighted_tokens     REAL    NOT NULL,
    output_tokens            INTEGER NOT NULL,
    turn_count               INTEGER NOT NULL,
    gap_s                    REAL    NOT NULL,
    tokens_per_pct_raw       REAL    NOT NULL,
    tokens_per_pct_cw        REAL    NOT NULL,
    UNIQUE (bucket, a_obs_id, b_obs_id)
);
CREATE INDEX idx_cal_bucket_ts ON calibration_points (account_id, bucket, b_ts_unix_ms);

CREATE TABLE limit_windows (
    account_id        INTEGER NOT NULL DEFAULT 1,
    bucket            TEXT    NOT NULL,
    start_unix_ms     INTEGER NOT NULL,
    end_unix_ms       INTEGER NOT NULL,
    reset_unix_ms     INTEGER NOT NULL DEFAULT 0,
    inferred          INTEGER NOT NULL DEFAULT 0,
    partial           INTEGER NOT NULL DEFAULT 0,
    in_progress       INTEGER NOT NULL DEFAULT 0,
    measured_pct      REAL    NOT NULL DEFAULT 0,
    attributed_pct    REAL    NOT NULL DEFAULT 0,
    peak_pct          INTEGER NOT NULL DEFAULT 0,
    hit_cap           INTEGER NOT NULL DEFAULT 0,
    tokens_per_pct_cw REAL    NOT NULL DEFAULT 0,
    PRIMARY KEY (account_id, bucket, start_unix_ms)
);
CREATE INDEX idx_limit_windows_start ON limit_windows (account_id, start_unix_ms);

-- session_uuid = '' stays the unattributed sentinel, now per account: meter
-- movement on this account that none of its ingested turns account for.
CREATE TABLE session_attribution (
    account_id           INTEGER NOT NULL DEFAULT 1,
    bucket               TEXT    NOT NULL,
    window_start_unix_ms INTEGER NOT NULL,
    session_uuid         TEXT    NOT NULL,
    project              TEXT    NOT NULL DEFAULT '',
    measured_pct         REAL    NOT NULL DEFAULT 0,
    estimated_pct        REAL    NOT NULL DEFAULT 0,
    cw_tokens            REAL    NOT NULL DEFAULT 0,
    raw_tokens           INTEGER NOT NULL DEFAULT 0,
    turn_count           INTEGER NOT NULL DEFAULT 0,
    first_ts_unix_ms     INTEGER NOT NULL DEFAULT 0,
    last_ts_unix_ms      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (account_id, bucket, window_start_unix_ms, session_uuid),
    FOREIGN KEY (account_id, bucket, window_start_unix_ms)
        REFERENCES limit_windows (account_id, bucket, start_unix_ms) ON DELETE CASCADE
);
CREATE INDEX idx_session_attr_session ON session_attribution (session_uuid, bucket);
CREATE INDEX idx_session_attr_project ON session_attribution (project, bucket);
CREATE INDEX idx_session_attr_window  ON session_attribution (account_id, bucket, window_start_unix_ms);

-- Events gain an account scope, the same move 0012 made for cwd. Meter facts
-- (a projection, a threshold band, saturation, collection health, a window
-- reset) are about one account's meter, and with two accounts the old key
-- would put both meters on one level track and flip it back and forth every
-- reconcile. 0 means "not account-scoped", as '' does for bucket, session and
-- cwd. Existing meter rows were all about the one meter there was, row 1.
ALTER TABLE events ADD COLUMN account_id INTEGER NOT NULL DEFAULT 0;
UPDATE events SET account_id = 1
 WHERE kind GLOB 'limit_projection.*' OR kind GLOB 'threshold.*'
    OR kind GLOB 'saturation.*' OR kind GLOB 'collection.*'
    OR kind = 'window.reset';
CREATE INDEX idx_events_account ON events (account_id);

CREATE TABLE event_levels_new (
  kind         TEXT    NOT NULL,
  bucket       TEXT    NOT NULL DEFAULT '',
  session_uuid TEXT    NOT NULL DEFAULT '',
  cwd          TEXT    NOT NULL DEFAULT '',
  account_id   INTEGER NOT NULL DEFAULT 0,
  state        TEXT    NOT NULL,
  since_ms     INTEGER NOT NULL,
  PRIMARY KEY (kind, bucket, session_uuid, cwd, account_id)
);
INSERT INTO event_levels_new (kind, bucket, session_uuid, cwd, account_id, state, since_ms)
  SELECT kind, bucket, session_uuid, cwd,
         CASE WHEN kind IN ('limit_projection', 'threshold', 'saturation', 'collection') THEN 1 ELSE 0 END,
         state, since_ms
    FROM event_levels;
DROP TABLE event_levels;
ALTER TABLE event_levels_new RENAME TO event_levels;

-- One-shot hooks can filter on an account. 0 matches any, so existing hooks
-- keep firing on whichever account's meter moves first.
ALTER TABLE event_hooks ADD COLUMN account_id INTEGER NOT NULL DEFAULT 0;
