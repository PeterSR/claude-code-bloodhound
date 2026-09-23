-- What the transcripts say about quota, as opposed to what the meter shows.
--
-- Everything bloodhound knew about the quota until now came from scraping the
-- /usage panel, which reports a percentage and a reset. A percentage is enough
-- right up to the moment it stops moving, and then it is ambiguous in a way
-- that matters: pinned at 100% can mean spend is flowing to the pay-per-use
-- tier, or that requests are being refused outright, or that the session
-- accepted Claude Code's offer to keep working at a lower request priority.
-- Those are three different situations for the person at the keyboard and the
-- panel renders them identically.
--
-- The transcript does distinguish them. A refused request is written down with
-- the server's own verdict attached: the exact reset moment, whether the
-- overage tier was available or refused and why, and whether the low priority
-- fallback was offered. A session that takes the offer gets its prompts queued
-- with a priority field set. This table is those records, kept because the
-- JSONL they came from is re-parsed rather than accumulated and because the
-- readers (the Now page, the weaverbird gauges, the announcer) need the answer
-- without opening transcripts of their own.
--
-- Rows are per session because that is who observed them, not because the
-- facts are per session. The refusal facts are account wide and any session
-- may be the one that hit the wall; the priority a session is queued at is
-- genuinely its own. Readers are written knowing which is which.
CREATE TABLE IF NOT EXISTS quota_signals (
  session_uuid            TEXT    NOT NULL,
  ts_unix_ms              INTEGER NOT NULL,
  -- 'rate_limited', 'low_priority_on' or 'low_priority_off'.
  kind                    TEXT    NOT NULL,
  -- 'session' | 'week' | '' , same vocabulary as the events log.
  bucket                  TEXT    NOT NULL DEFAULT '',
  -- When the refused window reopens, as a moment rather than a rendered
  -- time. Zero when the record did not carry one.
  reset_ts_unix_ms        INTEGER NOT NULL DEFAULT 0,
  overage_status          TEXT    NOT NULL DEFAULT '',
  overage_disabled_reason TEXT    NOT NULL DEFAULT '',
  using_overage           INTEGER NOT NULL DEFAULT 0,
  -- Rollout arm name rather than a bool, so an arm this build has never
  -- heard of does not read as "no offer was made".
  low_priority_offer      TEXT    NOT NULL DEFAULT '',
  project                 TEXT    NOT NULL DEFAULT '',
  cwd                     TEXT    NOT NULL DEFAULT '',
  PRIMARY KEY (session_uuid, ts_unix_ms, kind)
);

-- Every reader asks the same question: what is the newest signal, and is it
-- still about a window that has not reopened yet. Both halves are answered off
-- this index.
CREATE INDEX IF NOT EXISTS idx_quota_signals_ts ON quota_signals (ts_unix_ms);
