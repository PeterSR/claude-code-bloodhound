-- 0003: per-bucket saturation flags on usage_observations.
--
-- When a bucket reaches its cap (session at 100%, week at 100%) and the
-- user goes onto Anthropic's on-demand "Extra usage" tier, the pct stops
-- moving even though tokens keep being spent. Future tokens-per-1%
-- calibration must exclude those observations or it'll see infinite
-- ratios. We tag at write time so calibration queries are simple.
--
-- Threshold is 99 (not 100) because Anthropic's panel rounds — a value
-- of 99.4% displays as 99 but already behaves like a hit cap.

ALTER TABLE usage_observations
    ADD COLUMN session_saturated INTEGER NOT NULL DEFAULT 0;

ALTER TABLE usage_observations
    ADD COLUMN week_saturated INTEGER NOT NULL DEFAULT 0;

-- Backfill existing rows. Anything collected so far that hit ≥99 was
-- skewed in the same way; skip those when calibrating later.
UPDATE usage_observations
   SET session_saturated = 1
 WHERE session_pct IS NOT NULL AND session_pct >= 99;

UPDATE usage_observations
   SET week_saturated = 1
 WHERE week_pct IS NOT NULL AND week_pct >= 99;
