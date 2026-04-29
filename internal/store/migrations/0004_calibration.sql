-- 0004: tokens-per-1% calibration points.
--
-- Each row pairs two adjacent /usage observations within the same bucket
-- (no reset between, neither saturated) and divides the cost-weighted
-- tokens spent in that gap by the percentage points the bucket moved.
-- That ratio is "tokens per 1% of the bucket" — the closest thing we
-- have to translating Anthropic's opaque /usage percentage into raw
-- tokens.
--
-- Why a separate table instead of computing on read:
--   - The join (turns × observations) is moderately expensive; cache it.
--   - We want a stable history of how the ratio drifts over time.
--   - The aggregator already wipes-and-rebuilds derived tables, so this
--     fits the existing pattern.

CREATE TABLE calibration_points (
    id                       INTEGER PRIMARY KEY AUTOINCREMENT,
    bucket                   TEXT    NOT NULL,    -- 'session' | 'week'
    a_obs_id                 INTEGER NOT NULL,    -- earlier observation
    b_obs_id                 INTEGER NOT NULL,    -- later observation
    a_ts_unix_ms             INTEGER NOT NULL,
    b_ts_unix_ms             INTEGER NOT NULL,
    a_pct                    INTEGER NOT NULL,
    b_pct                    INTEGER NOT NULL,
    delta_pct                INTEGER NOT NULL,    -- b_pct - a_pct, always > 0
    raw_tokens               INTEGER NOT NULL,
    cost_weighted_tokens     REAL    NOT NULL,
    output_tokens            INTEGER NOT NULL,
    turn_count               INTEGER NOT NULL,
    gap_s                    REAL    NOT NULL,
    tokens_per_pct_raw       REAL    NOT NULL,    -- raw_tokens / delta_pct
    tokens_per_pct_cw        REAL    NOT NULL,    -- cost_weighted / delta_pct
    UNIQUE (bucket, a_obs_id, b_obs_id)
);

CREATE INDEX idx_cal_bucket_ts ON calibration_points (bucket, b_ts_unix_ms);
