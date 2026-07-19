-- Per-bucket reading validity.
--
-- A "valid" reading is one we trust as a real point on the usage curve. A
-- reading is marked invalid when the percentage falls further than integer
-- rounding can explain (more than 1 point below the window's running peak)
-- and then recovers on the next reading — the signature of a /usage
-- misparse, not a real dip. Downstream consumers (burn rate, calibration,
-- the usage chart) should ignore an invalid reading rather than draw a
-- cliff and a recovery that never happened.
--
-- Default 1: existing rows are trusted until RecomputeUsageFlags
-- reclassifies the whole series from history. That same pass also corrects
-- session_reset_detected / week_reset_detected, which a fixed 30-point drop
-- threshold previously under-counted (a real reset from 18% to 0% never
-- tripped it).
ALTER TABLE usage_observations ADD COLUMN session_pct_valid INTEGER NOT NULL DEFAULT 1;
ALTER TABLE usage_observations ADD COLUMN week_pct_valid    INTEGER NOT NULL DEFAULT 1;
