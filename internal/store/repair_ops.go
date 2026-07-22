package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// DefaultRepairWindowS is the measured sweet spot for the dedupe-history
// heuristic below. Validated against the 453 sessions whose transcript
// still exists (so the true per-response turn count is knowable) by
// comparing the heuristic's collapse to ground truth:
//
//	window   sessions matching exactly   net token error
//	2s       29%                         +36%
//	10s      70%                         +7.4%
//	60s      92%                         +0.19%
//
// 60s is the shipped default. It is exposed as --window-s because a wider
// or narrower window is a legitimate judgment call, not because 60 is
// wrong; see cmd/bloodhound/repair.go's Long help for the full tradeoff.
const DefaultRepairWindowS = 60.0

// RepairOptions configures DedupeHistory / ApplyDedupeHistory.
type RepairOptions struct {
	// WindowS is the largest gap, in seconds, between two adjacent turns
	// that still counts them as one duplicated API response. See
	// DefaultRepairWindowS for how this was measured.
	WindowS float64

	// All processes every session with turns, not just the ones whose
	// backing JSONL is gone. Sessions still on disk are better fixed
	// exactly by `bloodhound ingest --force` (it dedupes on the real
	// message.id/requestId, not a timing guess), so the default excludes
	// them and this heuristic only ever touches what ingest can't reach.
	All bool
}

// RepairStats summarises one DedupeHistory / ApplyDedupeHistory pass,
// dry run or applied.
type RepairStats struct {
	SessionsInScope int   `json:"sessions_in_scope"`
	SessionsChanged int   `json:"sessions_changed"`
	TurnsExamined   int   `json:"turns_examined"`
	RunsCollapsed   int   `json:"runs_collapsed"`
	TurnsRemoved    int   `json:"turns_removed"`
	RawTokensBefore int64 `json:"raw_tokens_before"`
	RawTokensAfter  int64 `json:"raw_tokens_after"`
}

// sessionRewrite is one session's fully renumbered turn set, ready to
// replace what's currently stored. Only produced for sessions where at
// least one run actually collapsed.
type sessionRewrite struct {
	SessionUUID string
	Turns       []TurnRow
}

// DedupeHistory computes, but does not write, a dedupe-history repair pass.
// This is the dry-run path; it also backs ApplyDedupeHistory's preview.
func (s *Store) DedupeHistory(ctx context.Context, opts RepairOptions) (RepairStats, error) {
	st, _, err := planDedupeHistory(ctx, s.DB, opts)
	return st, err
}

// ApplyDedupeHistory re-runs the same plan inside one write transaction and
// rewrites every changed session's turns, renumbering turn_idx densely from
// 0. Either every affected session lands, or (on any error) none does.
//
// Planning happens against the transaction itself, not s.DB, so the rows
// counted are exactly the rows rewritten even if something else touches
// the database between DedupeHistory's preview and this call.
func (s *Store) ApplyDedupeHistory(ctx context.Context, opts RepairOptions) (RepairStats, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return RepairStats{}, err
	}
	defer func() { _ = tx.Rollback() }()

	st, rewrites, err := planDedupeHistory(ctx, tx, opts)
	if err != nil {
		return RepairStats{}, err
	}
	for _, rw := range rewrites {
		if err := replaceSessionTurns(ctx, tx, rw.SessionUUID, rw.Turns); err != nil {
			return RepairStats{}, fmt.Errorf("rewrite session %s: %w", rw.SessionUUID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return RepairStats{}, err
	}
	return st, nil
}

// planDedupeHistory does the actual work shared by the dry run and the
// apply path: resolve which sessions are in scope, stream every turn in
// (session_uuid, turn_idx) order, and collapse each in-scope session's
// duplicate runs. q is execQuerier (defined in usage_flags.go) so the exact
// same code runs read-only against s.DB or read-write against the
// transaction that will also perform the rewrite.
func planDedupeHistory(ctx context.Context, q execQuerier, opts RepairOptions) (RepairStats, []sessionRewrite, error) {
	var st RepairStats

	scope, err := resolveRepairScope(ctx, q, opts.All)
	if err != nil {
		return st, nil, fmt.Errorf("resolve repair scope: %w", err)
	}
	st.SessionsInScope = len(scope)
	if len(scope) == 0 {
		return st, nil, nil
	}

	rows, err := q.QueryContext(ctx, `
		SELECT session_uuid, turn_idx, ts, ts_unix_ms, model,
		       input_tokens, output_tokens, cache_read,
		       cache_create_5m, cache_create_1h,
		       gap_s, classification, post_compact,
		       project, source_path_hash
		FROM turns
		ORDER BY session_uuid, turn_idx
	`)
	if err != nil {
		return st, nil, err
	}
	defer rows.Close()

	var (
		rewrites []sessionRewrite
		cur      []TurnRow
		curUUID  string
	)

	// flush processes the just-finished session's accumulated turns. Called
	// both mid-stream (when session_uuid changes) and once more after the
	// loop for the final session.
	flush := func() error {
		if curUUID == "" || len(cur) == 0 || !scope[curUUID] {
			return nil
		}
		st.TurnsExamined += len(cur)
		before := sumRawTokens(cur)
		st.RawTokensBefore += before

		collapsed, runs := collapseRuns(cur, opts.WindowS)
		st.RunsCollapsed += runs
		st.TurnsRemoved += len(cur) - len(collapsed)
		st.RawTokensAfter += sumRawTokens(collapsed)

		if len(collapsed) != len(cur) {
			st.SessionsChanged++
			rewrites = append(rewrites, sessionRewrite{
				SessionUUID: curUUID,
				Turns:       renumberTurns(collapsed),
			})
		}
		return nil
	}

	for rows.Next() {
		var t TurnRow
		var postCompact int
		if err := rows.Scan(&t.SessionUUID, &t.TurnIdx, &t.TS, &t.TSUnixMS, &t.Model,
			&t.InputTokens, &t.OutputTokens, &t.CacheRead,
			&t.CacheCreate5m, &t.CacheCreate1h,
			&t.GapS, &t.Classification, &postCompact,
			&t.Project, &t.SourcePathHash,
		); err != nil {
			return st, nil, err
		}
		t.PostCompact = postCompact != 0

		if t.SessionUUID != curUUID {
			if err := flush(); err != nil {
				return st, nil, err
			}
			curUUID = t.SessionUUID
			cur = cur[:0]
		}
		cur = append(cur, t)
	}
	if err := rows.Err(); err != nil {
		return st, nil, err
	}
	if err := flush(); err != nil {
		return st, nil, err
	}

	return st, rewrites, nil
}

// collapseRuns walks turns (already ordered by turn_idx for one session)
// and collapses each maximal run of adjacent turns that share the tuple
// duplicate API responses leave behind, within windowS seconds of each
// other, into a single row. It returns the collapsed sequence in the same
// order plus how many runs were collapsed (a run of length 1 doesn't count:
// nothing there needed collapsing).
//
// Which fields survive a collapse is deliberate, not arbitrary:
//   - ts / ts_unix_ms come from the LAST row, matching the fixed ingester's
//     dedupeLastIndex, which keeps the last occurrence (the one streaming
//     partial that isn't byte-identical to its duplicates carries the
//     finished usage only on its last line).
//   - gap_s / classification come from the FIRST row: only the first row of
//     a duplicate group carries the true gap from the previous real turn,
//     the rest have a gap near zero, which is exactly why classifications
//     like rotation/restructure were inflated by the original bug.
//   - post_compact is OR'd across the run so a flag on any row survives.
//   - model and every token column are identical across the run by
//     construction (that's the dedupe key), so first vs last is immaterial.
func collapseRuns(turns []TurnRow, windowS float64) ([]TurnRow, int) {
	if len(turns) == 0 {
		return nil, 0
	}
	windowMS := int64(windowS * 1000)

	out := make([]TurnRow, 0, len(turns))
	runsCollapsed := 0

	i := 0
	for i < len(turns) {
		j := i
		for j+1 < len(turns) && sameDedupeTuple(turns[j], turns[j+1]) &&
			withinWindow(turns[j].TSUnixMS, turns[j+1].TSUnixMS, windowMS) {
			j++
		}
		if j > i {
			out = append(out, collapseRun(turns[i:j+1]))
			runsCollapsed++
		} else {
			out = append(out, turns[i])
		}
		i = j + 1
	}
	return out, runsCollapsed
}

// sameDedupeTuple reports whether two turns carry the identical
// (model, input, output, cache_read, cache_create_5m, cache_create_1h)
// tuple that duplicate API responses share by construction.
func sameDedupeTuple(a, b TurnRow) bool {
	return a.Model == b.Model &&
		a.InputTokens == b.InputTokens &&
		a.OutputTokens == b.OutputTokens &&
		a.CacheRead == b.CacheRead &&
		a.CacheCreate5m == b.CacheCreate5m &&
		a.CacheCreate1h == b.CacheCreate1h
}

// withinWindow reports whether two ms timestamps are within windowMS of
// each other in either direction. Real transcripts are monotonic, but a
// clock oddity going the other way shouldn't break the chain on its own.
func withinWindow(aMS, bMS, windowMS int64) bool {
	d := bMS - aMS
	if d < 0 {
		d = -d
	}
	return d <= windowMS
}

// collapseRun folds a run of 1 or more turns into the single row that
// replaces them, per the field rules documented on collapseRuns.
func collapseRun(run []TurnRow) TurnRow {
	out := run[0]
	last := run[len(run)-1]
	out.TS = last.TS
	out.TSUnixMS = last.TSUnixMS
	for _, t := range run {
		if t.PostCompact {
			out.PostCompact = true
			break
		}
	}
	return out
}

// renumberTurns reassigns turn_idx densely from 0, preserving order. The
// (session_uuid, turn_idx) primary key must stay dense and gap-free after
// a collapse removes rows from the middle of the sequence.
func renumberTurns(turns []TurnRow) []TurnRow {
	out := make([]TurnRow, len(turns))
	for i, t := range turns {
		t.TurnIdx = i
		out[i] = t
	}
	return out
}

func sumRawTokens(turns []TurnRow) int64 {
	var sum int64
	for _, t := range turns {
		sum += int64(t.InputTokens) + int64(t.OutputTokens) +
			int64(t.CacheRead) + int64(t.CacheCreate5m) + int64(t.CacheCreate1h)
	}
	return sum
}

// resolveRepairScope returns the set of session_uuids DedupeHistory should
// touch. With all set, every session with turns qualifies. Otherwise only
// sessions whose backing JSONL is gone qualify: for each session we look up
// the path(s) its turns' source_path_hash resolves to via ingested_files
// and os.Stat each one. A session counts as "gone" only if none of its
// paths still exist (in practice a session has exactly one: the ingester
// names a session after its file's basename and always fully replaces a
// session's turns from a single file, but nothing here assumes that).
func resolveRepairScope(ctx context.Context, q execQuerier, all bool) (map[string]bool, error) {
	sessionHashes, err := distinctSessionSourceHashes(ctx, q)
	if err != nil {
		return nil, err
	}

	if all {
		scope := make(map[string]bool, len(sessionHashes))
		for uuid := range sessionHashes {
			scope[uuid] = true
		}
		return scope, nil
	}

	pathByHash, err := ingestedFilePaths(ctx, q)
	if err != nil {
		return nil, err
	}

	existsCache := map[string]bool{}
	pathExists := func(p string) bool {
		if p == "" {
			return false
		}
		if v, ok := existsCache[p]; ok {
			return v
		}
		_, statErr := os.Stat(p)
		v := statErr == nil
		existsCache[p] = v
		return v
	}

	scope := make(map[string]bool, len(sessionHashes))
	for uuid, hashes := range sessionHashes {
		stillOnDisk := false
		for h := range hashes {
			if pathExists(pathByHash[h]) {
				stillOnDisk = true
				break
			}
		}
		if !stillOnDisk {
			scope[uuid] = true
		}
	}
	return scope, nil
}

// distinctSessionSourceHashes maps each session_uuid to the set of
// source_path_hash values its turns carry.
func distinctSessionSourceHashes(ctx context.Context, q execQuerier) (map[string]map[string]bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT session_uuid, source_path_hash FROM turns`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]map[string]bool{}
	for rows.Next() {
		var uuid, hash string
		if err := rows.Scan(&uuid, &hash); err != nil {
			return nil, err
		}
		if out[uuid] == nil {
			out[uuid] = map[string]bool{}
		}
		out[uuid][hash] = true
	}
	return out, rows.Err()
}

func ingestedFilePaths(ctx context.Context, q execQuerier) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT path_hash, path FROM ingested_files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var hash, path string
		if err := rows.Scan(&hash, &path); err != nil {
			return nil, err
		}
		out[hash] = path
	}
	return out, rows.Err()
}

// replaceSessionTurns rewrites exactly one session's turns rows, leaving
// compactions, user_prompts and every other table untouched. This is
// deliberately narrower than ReplaceSessionData (which wipes and rebuilds
// turns + compactions + user_prompts together for a freshly ingested file):
// a heuristic repair has no compaction or prompt data to write back, so
// reusing that wider helper would silently delete compactions this session
// already has.
func replaceSessionTurns(ctx context.Context, tx *sql.Tx, sessionUUID string, turns []TurnRow) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM turns WHERE session_uuid = ?`, sessionUUID); err != nil {
		return err
	}
	if len(turns) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO turns (
			session_uuid, turn_idx, ts, ts_unix_ms, model,
			input_tokens, output_tokens, cache_read,
			cache_create_5m, cache_create_1h,
			gap_s, classification, post_compact,
			project, source_path_hash
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, t := range turns {
		pc := 0
		if t.PostCompact {
			pc = 1
		}
		if _, err := stmt.ExecContext(ctx,
			t.SessionUUID, t.TurnIdx, t.TS, t.TSUnixMS, t.Model,
			t.InputTokens, t.OutputTokens, t.CacheRead,
			t.CacheCreate5m, t.CacheCreate1h,
			t.GapS, t.Classification, pc,
			t.Project, t.SourcePathHash,
		); err != nil {
			return err
		}
	}
	return nil
}

// BackupPath returns the path Backup would write to for the given time,
// matching the "bloodhound.db.backup-YYYYMMDD-HHMM" naming an earlier
// one-off manual backup already uses in the wild, so both look like the
// same kind of file to a user browsing their data directory. Minute
// resolution means two --apply runs in the same minute produce the same
// name; see NextBackupPath for how that's resolved without touching this
// convention.
func (s *Store) BackupPath(at time.Time) string {
	return s.Path + ".backup-" + at.Format("20060102-1504")
}

// maxBackupNameAttempts bounds NextBackupPath's search for a free name so a
// directory full of stale numbered backups (or a bug re-running --apply in
// a loop) can't spin forever; that many collisions in one minute is not a
// case worth serving, only one worth failing loudly on.
const maxBackupNameAttempts = 1000

// NextBackupPath returns an unused path to back up to for the given time.
// It starts from BackupPath's minute-resolution name and, if that's taken,
// appends a numeric ".2", ".3", ... suffix until it finds one that isn't.
//
// A suffix was chosen over dropping to second (or finer) resolution because
// the minute-grained name is an established convention: a real user's data
// directory already holds a manually-made bloodhound.db.backup-20260604-0923
// from before this command existed. Widening the timestamp itself would
// change what every future backup looks like just to dodge a collision that
// is rare and only matters when it happens; a suffix keeps the common case
// (one --apply per minute) looking exactly as before and only grows the
// name when a second run actually needs it.
func (s *Store) NextBackupPath(at time.Time) (string, error) {
	base := s.BackupPath(at)
	if free, err := pathIsFree(base); err != nil {
		return "", err
	} else if free {
		return base, nil
	}
	for n := 2; n <= maxBackupNameAttempts; n++ {
		candidate := fmt.Sprintf("%s.%d", base, n)
		free, err := pathIsFree(candidate)
		if err != nil {
			return "", err
		}
		if free {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no free backup filename after %d beside %s, giving up rather than searching forever", maxBackupNameAttempts, base)
}

// pathIsFree reports whether p does not yet exist, distinguishing "does not
// exist" from a stat error worth surfacing (permissions, a bad mount, ...).
func pathIsFree(p string) (bool, error) {
	if _, err := os.Stat(p); err == nil {
		return false, nil
	} else if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	} else {
		return false, err
	}
}

// Backup writes a full, transactionally consistent snapshot of the live
// database to dst via SQLite's VACUUM INTO. That handles a database open in
// WAL mode correctly (a raw file copy can capture a torn mid-checkpoint
// state); it also means no dependency on the sqlite3 CLI being installed.
// dst must not already exist, since VACUUM INTO refuses to overwrite one
// that does.
func (s *Store) Backup(ctx context.Context, dst string) error {
	if free, err := pathIsFree(dst); err != nil {
		return err
	} else if !free {
		return fmt.Errorf("backup destination already exists: %s", dst)
	}
	if _, err := s.DB.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		return fmt.Errorf("backup to %s: %w", dst, err)
	}
	return nil
}
