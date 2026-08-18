-- Sessions bloodhound promised to write to again when a window reopens.
--
-- The wakeup nudge that shipped before this only ever suggested: it told a
-- session the work could pick up when the window reopened and left arming
-- something to the reader. A project can now ask bloodhound to carry the
-- wakeup itself, which needs a memory that outlives the message. The session
-- is noted down at the moment it is told to stop, and the row is what the
-- reconcile pass reads later to know who to write to and when.
--
-- Durable rather than in-process because the whole point is the gap. The
-- daemon may restart, the machine may suspend, and the reset it is waiting
-- for can be hours out; a promise that only survives while the process does
-- is one that breaks exactly when a five hour window is involved.
--
-- due_ms is stored as a moment rather than the reset_ts string the event
-- carried, so firing is a numeric comparison and nothing has to re-parse a
-- timestamp on every tick. expire_ms is when the promise stops being worth
-- keeping: waking a session hours after the window reopened is not resuming
-- work, it is an interruption with a stale reason attached.
CREATE TABLE IF NOT EXISTS session_wakeups (
  id           INTEGER PRIMARY KEY,
  session_uuid TEXT    NOT NULL,
  pid          INTEGER NOT NULL DEFAULT 0,
  cwd          TEXT    NOT NULL DEFAULT '',
  bucket       TEXT    NOT NULL,
  -- What was said at the time, so the message on the far side can name the
  -- reason rather than announcing a reset out of nowhere.
  reason       TEXT    NOT NULL DEFAULT '',
  armed_ms     INTEGER NOT NULL,
  due_ms       INTEGER NOT NULL,
  expire_ms    INTEGER NOT NULL,
  attempts     INTEGER NOT NULL DEFAULT 0,
  -- '' while pending, then 'delivered', 'expired' or 'superseded'.
  outcome      TEXT    NOT NULL DEFAULT '',
  resolved_ms  INTEGER,
  note         TEXT    NOT NULL DEFAULT ''
);

-- One pending promise per session and window. A second warning about the same
-- window before the first came due is the same promise with a fresher
-- deadline, not a second wakeup, and without this the session would be
-- written to twice for one stop. Partial rather than plain, because the
-- resolved rows are history and several of them for the same pair are normal.
CREATE UNIQUE INDEX IF NOT EXISTS idx_session_wakeups_pending
  ON session_wakeups (session_uuid, bucket) WHERE outcome = '';

-- The reconcile pass asks one question every minute: is anything due. Keeping
-- the pending rows indexed by deadline is what makes that question free on an
-- install with months of resolved history behind it.
CREATE INDEX IF NOT EXISTS idx_session_wakeups_due
  ON session_wakeups (due_ms) WHERE outcome = '';
