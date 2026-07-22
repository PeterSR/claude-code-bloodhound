-- Per-session attribution of the /usage limit meters.
--
-- Both tables are derived: the aggregator wipes and rebuilds them from
-- usage_observations + turns on every run, exactly like buckets and
-- calibration_points. Nothing here is a source of truth, so a schema change
-- later costs nothing but a re-aggregate.
--
-- `bucket` is '5h' | 'week', NOT calibration_points' 'session' | 'week'.
-- In this feature "session" always means a Claude Code conversation
-- (session_uuid); the 5-hour limit window needed a name that couldn't be
-- confused with one.

-- One row per reconstructed limit window. Windows are identified by the
-- reset timestamp the /usage panel advertises: consecutive observations
-- counting down to the same reset are one window, and a new window opens
-- only when that reset jumps forward far enough to be a genuine rotation
-- rather than a misparse (same rule the weekly-capacity view uses).
CREATE TABLE limit_windows (
    bucket          TEXT    NOT NULL,        -- '5h' | 'week'
    start_unix_ms   INTEGER NOT NULL,
    end_unix_ms     INTEGER NOT NULL,
    reset_unix_ms   INTEGER NOT NULL DEFAULT 0,  -- advertised reset that closes it; 0 = unknown
    inferred        INTEGER NOT NULL DEFAULT 0,  -- 1 = synthesized on a fixed grid, no observations covered it
    partial         INTEGER NOT NULL DEFAULT 0,  -- 1 = the series began mid-window, so its total under-counts
    in_progress     INTEGER NOT NULL DEFAULT 0,  -- 1 = the newest window, still filling
    measured_pct    REAL    NOT NULL DEFAULT 0,  -- meter movement actually observed inside this window
    attributed_pct  REAL    NOT NULL DEFAULT 0,  -- sum of every session's share (measured + estimated)
    peak_pct        INTEGER NOT NULL DEFAULT 0,  -- highest reading seen in the window
    hit_cap         INTEGER NOT NULL DEFAULT 0,  -- 1 = some reading was saturated (>=99%)

    -- What one point of this meter cost here, reconciled over the whole
    -- window: covered cost-weighted tokens / measured_pct. 0 when the window
    -- didn't move enough to price anything.
    --
    -- Note this is NOT calibration_points.tokens_per_pct_cw. That one is
    -- computed per observation pair and only from pairs where the displayed
    -- integer ticked, which drops every flat interval's tokens and comes out
    -- several times too cheap. See .agent-workspace/NOTES.md.
    tokens_per_pct_cw REAL NOT NULL DEFAULT 0,

    PRIMARY KEY (bucket, start_unix_ms)
);

CREATE INDEX idx_limit_windows_start ON limit_windows (start_unix_ms);

-- One session's share of one limit window.
--
-- session_uuid = '' is the UNATTRIBUTED sentinel: meter movement no ingested
-- turn accounts for (a second machine on the same account, a JSONL we never
-- read). Kept as a real row rather than normalized away so the UI can show
-- the gap instead of quietly inflating everyone's share to cover it.
--
-- measured_pct comes from observed meter movement split pro-rata by
-- cost-weighted tokens; estimated_pct comes from the calibration median for
-- spans no pair of observations bracketed. They are stored apart so a
-- consumer can require the measured kind.
CREATE TABLE session_attribution (
    bucket               TEXT    NOT NULL,   -- '5h' | 'week'
    window_start_unix_ms INTEGER NOT NULL,
    session_uuid         TEXT    NOT NULL,   -- '' = unattributed remainder
    project              TEXT    NOT NULL DEFAULT '',
    measured_pct         REAL    NOT NULL DEFAULT 0,
    estimated_pct        REAL    NOT NULL DEFAULT 0,
    cw_tokens            REAL    NOT NULL DEFAULT 0,
    raw_tokens           INTEGER NOT NULL DEFAULT 0,
    turn_count           INTEGER NOT NULL DEFAULT 0,
    first_ts_unix_ms     INTEGER NOT NULL DEFAULT 0,
    last_ts_unix_ms      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (bucket, window_start_unix_ms, session_uuid),
    FOREIGN KEY (bucket, window_start_unix_ms)
        REFERENCES limit_windows (bucket, start_unix_ms) ON DELETE CASCADE
);

CREATE INDEX idx_session_attr_session ON session_attribution (session_uuid, bucket);
CREATE INDEX idx_session_attr_project ON session_attribution (project, bucket);
CREATE INDEX idx_session_attr_window  ON session_attribution (bucket, window_start_unix_ms);
