-- Link subagent transcripts to the session that dispatched them.
--
-- Claude Code writes subagent (Task-tool) transcripts one level deeper than
-- top-level sessions, at <project>/<parent-uuid>/subagents/agent-*.jsonl.
-- Before this migration internal/ingest never looked there at all, so a
-- supervisor session that dispatched implementation agents was credited
-- only for its own turns and read far cheaper than it actually was.
--
-- parent_session_uuid links a subagent's own session_uuid back to its
-- dispatcher so rollups can fold the two together
-- (COALESCE(NULLIF(parent_session_uuid,''), session_uuid), the "effective
-- owner"), while turns/session_attribution stay keyed by the actual session
-- that spent, so per-subagent detail is never lost.
--
-- cwd is the record's own working directory. turns.project already holds
-- the sanitized project dir name (e.g. "-home-peter-dev-personal-cad-web"),
-- but that sanitization just replaces "/" with "-" and can't be reversed
-- unambiguously when a real path segment legitimately contains a hyphen.
-- A subagent's cwd can also legitimately differ from its parent's project
-- directory (observed live: a subagent working in a subdirectory of the
-- same repo), so it needs to be its own column rather than assumed equal
-- to the parent's.
--
-- Backfill: neither column is derivable from data already in these tables.
-- parent_session_uuid didn't exist as a concept before this migration (every
-- row ingested so far is a top-level session, so '' is the correct value,
-- not a placeholder), and cwd was parsed off every JSONL record by
-- internal/ingest all along but never persisted, so there is nothing on
-- disk in the database to recover it from. Both columns stay at their
-- DEFAULT '' for existing rows until the source .jsonl is re-ingested
-- (`bloodhound ingest --force`); a session whose transcript has since
-- rotated off disk keeps '' forever, the same limit every other
-- disk-dependent repair in this codebase has.
ALTER TABLE turns ADD COLUMN parent_session_uuid TEXT NOT NULL DEFAULT '';
ALTER TABLE turns ADD COLUMN cwd                 TEXT NOT NULL DEFAULT '';

ALTER TABLE sessions ADD COLUMN parent_session_uuid TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN cwd                 TEXT NOT NULL DEFAULT '';

-- The rollup helpers in internal/store/attribution_ops.go join
-- session_attribution to sessions on the primary key (already indexed) to
-- read parent_session_uuid per row; this index serves the reverse lookup
-- ("which sessions did X dispatch"), which is the natural query shape for
-- this column and cheap to maintain given how small the sessions table is
-- relative to turns.
CREATE INDEX idx_sessions_parent ON sessions (parent_session_uuid);
