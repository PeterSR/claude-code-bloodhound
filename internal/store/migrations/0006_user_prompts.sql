-- User prompts: one row per human-typed user message in a session.
-- We keep only a short text preview (truncated upstream); never the full
-- message body. Used by the Now page to disambiguate cards that share a
-- project name.

CREATE TABLE user_prompts (
    session_uuid TEXT    NOT NULL,
    ts_unix_ms   INTEGER NOT NULL,
    text_preview TEXT    NOT NULL,
    PRIMARY KEY (session_uuid, ts_unix_ms)
);

CREATE INDEX idx_user_prompts_session_ts
    ON user_prompts (session_uuid, ts_unix_ms DESC);
