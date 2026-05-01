-- 0005: backfill reset_detected for boundary-crossing observations.
--
-- The original detector in RecordUsage only flagged a reset when pct
-- dropped by ≥30pp between adjacent polls. That misses cases where the
-- daemon was offline (or the user idle) across a reset boundary and the
-- post-reset accumulation was already non-trivial when the next poll
-- landed: prev=40%, gap, current=20% — clearly a reset, but the 20pp
-- delta is below threshold. The History chart then drew a misleading
-- diagonal across the rotation.
--
-- This migration uses LAG() over ts_unix_ms to flag any observation
-- whose timestamp falls at or past the preceding observation's
-- session_reset_ts / week_reset_ts. RecordUsage applies the same rule
-- going forward.

UPDATE usage_observations
   SET session_reset_detected = 1
 WHERE id IN (
     SELECT id
       FROM (
         SELECT id,
                ts,
                session_reset_detected,
                session_pct,
                LAG(session_pct)       OVER (ORDER BY ts_unix_ms) AS prev_session_pct,
                LAG(session_reset_ts)  OVER (ORDER BY ts_unix_ms) AS prev_session_reset_ts
           FROM usage_observations
       )
      WHERE session_reset_detected = 0
        AND session_pct IS NOT NULL
        AND prev_session_pct IS NOT NULL
        AND prev_session_reset_ts IS NOT NULL
        AND julianday(ts) >= julianday(prev_session_reset_ts)
 );

UPDATE usage_observations
   SET week_reset_detected = 1
 WHERE id IN (
     SELECT id
       FROM (
         SELECT id,
                ts,
                week_reset_detected,
                week_pct,
                LAG(week_pct)        OVER (ORDER BY ts_unix_ms) AS prev_week_pct,
                LAG(week_reset_ts)   OVER (ORDER BY ts_unix_ms) AS prev_week_reset_ts
           FROM usage_observations
       )
      WHERE week_reset_detected = 0
        AND week_pct IS NOT NULL
        AND prev_week_pct IS NOT NULL
        AND prev_week_reset_ts IS NOT NULL
        AND julianday(ts) >= julianday(prev_week_reset_ts)
 );
