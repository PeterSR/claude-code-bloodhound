-- One-shot hooks gain a cwd filter, mirroring the scope 0012 added to events
-- and event_levels.
--
-- Without it a peer wanting "run this when THIS directory's budget goes tight"
-- can only match budget.* across every directory on the machine, which for the
-- one kind of event that is directory-scoped makes the filter useless. Existing
-- hooks get '', which means "any directory", the same as the empty bucket and
-- session filters already beside it.
ALTER TABLE event_hooks ADD COLUMN cwd TEXT NOT NULL DEFAULT '';
