package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// LimitWindowRow mirrors the `limit_windows` table.
type LimitWindowRow struct {
	Bucket        string
	StartUnixMS   int64
	EndUnixMS     int64
	ResetUnixMS   int64
	Inferred      bool
	Partial       bool
	InProgress    bool
	MeasuredPct   float64
	AttributedPct float64
	PeakPct       int
	HitCap        bool
	// TokensPerPctCW is what one point of this meter cost in this window,
	// reconciled over the whole window. 0 when it didn't move enough.
	TokensPerPctCW float64
}

// AttributionRow mirrors the `session_attribution` table.
type AttributionRow struct {
	Bucket            string
	WindowStartUnixMS int64
	SessionUUID       string // "" = the unattributed remainder
	Project           string
	MeasuredPct       float64
	EstimatedPct      float64
	CWTokens          float64
	RawTokens         int64
	TurnCount         int
	FirstTSUnixMS     int64
	LastTSUnixMS      int64
	// EffectiveSessionUUID is SessionUUID unless this row belongs to a
	// subagent, in which case it's the parent that dispatched it (the same
	// COALESCE(NULLIF(parent_session_uuid,''), session_uuid) rule
	// SessionPctTotalsAll and GroupAttribution already apply). Only
	// WindowSlices populates it; every other producer of AttributionRow
	// leaves it at its zero value because nothing downstream of them reads
	// it.
	EffectiveSessionUUID string
	// Cwd is the EFFECTIVE owner's cwd, the same resolution
	// AttributionGroup.Cwd and SessionPctTotals.Cwd already apply (a
	// subagent's own cwd never surfaces here, only its dispatcher's). Only
	// WindowSlices populates it, same restriction as EffectiveSessionUUID:
	// the web stacked-window chart needs it to key a window's slices by
	// directory when grouping by=="cwd", the same way GroupAttribution
	// keys its rollup. "" covers two different rows that this field alone
	// can't tell apart - the unattributed remainder (SessionUUID == "") and
	// a real session whose cwd was never captured - a caller that needs the
	// distinction already has SessionUUID for it.
	Cwd string
}

// ReplaceAttribution wipes and re-inserts one bucket's windows and session
// rows in a single transaction. Scoped per bucket so a failure rebuilding
// the weekly view doesn't take the 5h view down with it.
//
// Children go first and parents last on the way out, parents first on the
// way in: session_attribution carries a real foreign key into limit_windows
// and the store opens SQLite with foreign_keys on.
func (s *Store) ReplaceAttribution(ctx context.Context, bucket string, windows []LimitWindowRow, rows []AttributionRow) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM session_attribution WHERE bucket = ?`, bucket); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM limit_windows WHERE bucket = ?`, bucket); err != nil {
		return err
	}

	wstmt, err := tx.PrepareContext(ctx, `
		INSERT INTO limit_windows (
			bucket, start_unix_ms, end_unix_ms, reset_unix_ms,
			inferred, partial, in_progress,
			measured_pct, attributed_pct, peak_pct, hit_cap,
			tokens_per_pct_cw
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
	`)
	if err != nil {
		return err
	}
	defer wstmt.Close()
	for _, w := range windows {
		if _, err := wstmt.ExecContext(ctx,
			w.Bucket, w.StartUnixMS, w.EndUnixMS, w.ResetUnixMS,
			b2i(w.Inferred), b2i(w.Partial), b2i(w.InProgress),
			w.MeasuredPct, w.AttributedPct, w.PeakPct, b2i(w.HitCap),
			w.TokensPerPctCW,
		); err != nil {
			return err
		}
	}

	rstmt, err := tx.PrepareContext(ctx, `
		INSERT INTO session_attribution (
			bucket, window_start_unix_ms, session_uuid, project,
			measured_pct, estimated_pct,
			cw_tokens, raw_tokens, turn_count,
			first_ts_unix_ms, last_ts_unix_ms
		) VALUES (?,?,?,?,?,?,?,?,?,?,?)
	`)
	if err != nil {
		return err
	}
	defer rstmt.Close()
	for _, r := range rows {
		if _, err := rstmt.ExecContext(ctx,
			r.Bucket, r.WindowStartUnixMS, r.SessionUUID, r.Project,
			r.MeasuredPct, r.EstimatedPct,
			r.CWTokens, r.RawTokens, r.TurnCount,
			r.FirstTSUnixMS, r.LastTSUnixMS,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SessionPctTotals is one session's rolled-up cost across every limit window
// it touched.
type SessionPctTotals struct {
	SessionUUID string
	Project     string
	// Cwd is the EFFECTIVE owner's working directory: for a session that
	// dispatched subagents, its own cwd (never a subagent's, even though a
	// subagent's own spend is folded into this same row - see
	// SessionPctTotalsAll). A subagent looked up directly never has its own
	// key in the map SessionPctTotalsAll returns (its spend lives under its
	// parent's key instead), so this field is never a subagent's cwd by
	// construction, not just by convention. Empty when the underlying
	// session's cwd was never captured (transcript rotated off disk before
	// this column existed).
	Cwd string

	// WeekPct is the session's share of the weekly limit. Additive and
	// well-behaved: every session in a week sums to that week's usage.
	WeekPct float64
	// FiveHPct is the share of the 5-hour limit summed across every window
	// the session spanned, so a long session legitimately exceeds 100: read
	// it as "1.4 five-hour budgets". FiveHPeakPct is its single worst window,
	// which is the number bounded by 100.
	FiveHPct     float64
	FiveHPeakPct float64

	// MeasuredPct / EstimatedPct split WeekPct by how it was derived, so a
	// caller can tell a number anchored to real meter movement from one the
	// calibration median produced.
	MeasuredPct  float64
	EstimatedPct float64

	Windows5h  int
	WindowWeek int
}

// SessionPctTotalsAll returns the rollup for every session that has any
// attribution, keyed by the EFFECTIVE owner: a subagent's spend folds into
// whichever session dispatched it (COALESCE(NULLIF(parent_session_uuid, ""),
// session_uuid)), so a supervisor's total includes the subagents it ran and
// a subagent looked up by its own UUID is not a separate key here.
// session_attribution itself stays keyed by the actual spending session
// (joined in below via sessions.parent_session_uuid), so that detail isn't
// lost, just rolled up one level for this view.
func (s *Store) SessionPctTotalsAll(ctx context.Context) (map[string]*SessionPctTotals, error) {
	// per_window sums parent + subagent shares that landed in the SAME
	// window before the outer query takes MAX() for the peak: doing that in
	// one flat GROUP BY would instead take the largest single (session,
	// window) row, understating a supervisor's peak by however much its
	// subagents contributed alongside it in that window (the common case,
	// since a supervisor typically dispatches subagents inside one 5h
	// window rather than across a boundary).
	// psess resolves the effective owner's OWN sessions row, so cwd (unlike
	// project) can't just read sa.project straight off session_attribution:
	// a subagent's cwd genuinely differs from its dispatcher's, and this
	// rollup's whole point is to report the dispatcher's, never the
	// subagent's, for a row that already folds the subagent's spend in.
	rows, err := s.DB.QueryContext(ctx, `
		WITH per_window AS (
			SELECT
			    COALESCE(NULLIF(sess.parent_session_uuid, ''), sa.session_uuid) AS effective_uuid,
			    sa.project              AS project,
			    COALESCE(CASE WHEN sess.parent_session_uuid <> '' THEN psess.cwd ELSE sess.cwd END, '') AS cwd,
			    sa.bucket               AS bucket,
			    sa.window_start_unix_ms AS window_start_unix_ms,
			    SUM(sa.measured_pct)    AS measured,
			    SUM(sa.estimated_pct)   AS estimated
			FROM session_attribution sa
			LEFT JOIN sessions sess  ON sess.session_uuid = sa.session_uuid
			LEFT JOIN sessions psess ON psess.session_uuid = sess.parent_session_uuid
			WHERE sa.session_uuid <> ''
			GROUP BY effective_uuid, sa.bucket, sa.window_start_unix_ms
		)
		SELECT effective_uuid, MAX(project) AS project, MAX(cwd) AS cwd, bucket,
		       SUM(measured + estimated) AS pct,
		       MAX(measured + estimated) AS peak,
		       SUM(measured)  AS measured,
		       SUM(estimated) AS estimated,
		       COUNT(*)       AS windows
		FROM per_window
		GROUP BY effective_uuid, bucket
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]*SessionPctTotals{}
	for rows.Next() {
		var (
			uuid, project, cwd, bucket  string
			pct, peak, measured, estPct float64
			windows                     int
		)
		if err := rows.Scan(&uuid, &project, &cwd, &bucket, &pct, &peak, &measured, &estPct, &windows); err != nil {
			return nil, err
		}
		t, ok := out[uuid]
		if !ok {
			t = &SessionPctTotals{SessionUUID: uuid, Project: project, Cwd: cwd}
			out[uuid] = t
		}
		switch bucket {
		case "week":
			t.WeekPct = pct
			t.MeasuredPct = measured
			t.EstimatedPct = estPct
			t.WindowWeek = windows
		default:
			t.FiveHPct = pct
			t.FiveHPeakPct = peak
			t.Windows5h = windows
		}
	}
	return out, rows.Err()
}

// SessionPctWindows returns one session's per-window slices for a bucket,
// oldest first: the "over time" view of a single conversation.
//
// uuid is matched two ways at once, unioned by the WHERE clause below rather
// than picked between: as the EFFECTIVE owner (COALESCE(NULLIF(
// parent_session_uuid,”), session_uuid), the same fold SessionPctTotalsAll
// and GroupAttribution already apply), so a supervisor's array includes the
// subagents it dispatched instead of only its own direct rows; and as the
// row's own raw session_uuid, so a subagent looked up by its own uuid still
// gets its own unfolded detail, the one reachability path
// "attribution windows --slices" and "attribution --by session" promise
// (they fold subagents away, and point back here for their own view).
// A subagent never dispatches further subagents in this schema (parent_
// session_uuid always names the top-level session, see the workflow-agent
// ingest comment), so the two conditions can't both match distinct rows for
// the same call and double-count: querying a parent folds its children in,
// querying a child returns only itself.
//
// Before this fold, totals.week_pct (from SessionPctTotalsAll) and the sum
// of week[].pct here could disagree by however much the session's subagents
// spent, which is exactly the bug: a supervisor with 114 subagents reported
// 9.70% here against a totals.week_pct of 38.45%. Grouping by window here
// (rather than returning one row per contributing session_uuid) is what
// collapses those 114 rows back down to the single window they share.
func (s *Store) SessionPctWindows(ctx context.Context, uuid, bucket string) ([]AttributionRow, []LimitWindowRow, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT a.window_start_unix_ms, MAX(a.project) AS project,
		       SUM(a.measured_pct) AS measured_pct, SUM(a.estimated_pct) AS estimated_pct,
		       SUM(a.cw_tokens) AS cw_tokens, SUM(a.raw_tokens) AS raw_tokens, SUM(a.turn_count) AS turn_count,
		       MIN(NULLIF(a.first_ts_unix_ms, 0)) AS first_ts_unix_ms, MAX(a.last_ts_unix_ms) AS last_ts_unix_ms,
		       w.end_unix_ms, w.inferred, w.partial, w.in_progress,
		       w.measured_pct, w.attributed_pct, w.peak_pct, w.hit_cap,
		       w.tokens_per_pct_cw
		FROM session_attribution a
		LEFT JOIN sessions sess ON sess.session_uuid = a.session_uuid
		JOIN limit_windows w
		  ON w.bucket = a.bucket AND w.start_unix_ms = a.window_start_unix_ms
		WHERE a.bucket = ?
		  AND (COALESCE(NULLIF(sess.parent_session_uuid, ''), a.session_uuid) = ? OR a.session_uuid = ?)
		GROUP BY a.window_start_unix_ms
		ORDER BY a.window_start_unix_ms ASC
	`, bucket, uuid, uuid)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var (
		out  []AttributionRow
		wins []LimitWindowRow
	)
	for rows.Next() {
		r := AttributionRow{Bucket: bucket, SessionUUID: uuid}
		w := LimitWindowRow{Bucket: bucket}
		var inferred, partial, inProgress, hitCap int
		var firstTS sql.NullInt64
		if err := rows.Scan(
			&r.WindowStartUnixMS, &r.Project,
			&r.MeasuredPct, &r.EstimatedPct,
			&r.CWTokens, &r.RawTokens, &r.TurnCount,
			&firstTS, &r.LastTSUnixMS,
			&w.EndUnixMS, &inferred, &partial, &inProgress,
			&w.MeasuredPct, &w.AttributedPct, &w.PeakPct, &hitCap,
			&w.TokensPerPctCW,
		); err != nil {
			return nil, nil, err
		}
		r.FirstTSUnixMS = firstTS.Int64
		w.StartUnixMS = r.WindowStartUnixMS
		w.Inferred, w.Partial, w.InProgress, w.HitCap = inferred == 1, partial == 1, inProgress == 1, hitCap == 1
		out = append(out, r)
		wins = append(wins, w)
	}
	return out, wins, rows.Err()
}

// AttributionGroup is a rollup over one grouping key (a project, a session,
// or a working directory) within a time range.
type AttributionGroup struct {
	Key           string
	Project       string
	Pct           float64
	MeasuredPct   float64
	EstimatedPct  float64
	PeakPct       float64
	CWTokens      float64
	RawTokens     int64
	TurnCount     int
	Sessions      int
	Windows       int
	FirstTSUnixMS int64
	LastTSUnixMS  int64
	// Cwd is the group's working directory, populated for by=="session" and
	// by=="cwd" (where it's simply the same value as Key). A project-keyed
	// group deliberately gets "" instead of an arbitrary member's directory:
	// the whole point of adding this field is that one project can span many
	// directories (measured on a real account: 140 distinct cwds under 44
	// projects), so picking just one to show would misrepresent the rest
	// rather than fill a gap. Empty here for a by=="cwd" group specifically
	// means Key is either the unattributed sentinel ("") or UnknownCwd: a
	// real directory value is never empty (see UnknownCwd's doc).
	Cwd string
}

// UnknownCwd is the group key GroupAttribution's by=="cwd" mode uses for a
// row whose effective owner is a real, known session (or supervisor plus
// subagents) but whose cwd was never captured (transcript rotated off disk
// before the column existed, same gap SessionPctTotals.Cwd and
// AttributionGroup.Cwd on other groupings already document).
//
// Deliberately not "": that's attribute.Unattributed, a different fact
// (meter movement no turn of ours explains, not tied to any session at
// all). Collapsing the two into one key would make "we know exactly who
// spent it, just not where" indistinguishable from "nothing we ingested
// explains this spend" on the wire, which is the one thing this bucket
// exists to avoid.
//
// Not a valid absolute path (no leading "/"), so it can never collide with
// a real cwd: every cwd this codebase stores is one.
const UnknownCwd = "__unknown_cwd__"

// GroupAttribution rolls attribution up by project, by session, or by
// absolute working directory over the windows that start at or after
// sinceMS. by is "project", "session", or "cwd".
//
// by=="session" groups by the EFFECTIVE owner (COALESCE(NULLIF(
// parent_session_uuid, ""), session_uuid)) rather than the raw session_uuid,
// so a supervisor's row includes the subagents it dispatched instead of
// listing them as its peers. by=="project" is unaffected: a subagent already
// carries its parent's project (see internal/ingest), so grouping by project
// naturally already pools them, join or no join. by=="cwd" groups by that
// same effective owner's own cwd (never a subagent's), so a supervisor and
// everything it dispatched fold into whichever directory the supervisor
// itself ran in - the same rule, applied to directory instead of session id.
func (s *Store) GroupAttribution(ctx context.Context, bucket, by string, sinceMS int64) ([]AttributionGroup, error) {
	// effCwdExpr resolves to the EFFECTIVE owner's cwd, never a subagent's:
	// when the spending session has a parent, its own cwd is irrelevant to
	// this group (that's the row this whole rollup already folds it into),
	// so we reach one more join to the parent's own sessions row instead of
	// reporting where the subagent itself happened to run. Shared by
	// by=="session" (where it becomes the Cwd column) and by=="cwd" (where
	// it becomes the group's key too), so there is exactly one definition of
	// "the effective owner's cwd" in this file, not two that could drift.
	effCwdExpr := "COALESCE(CASE WHEN sess.parent_session_uuid <> '' THEN psess.cwd ELSE sess.cwd END, '')"

	keyCol := "sa.project"
	// cwdExpr populates the Cwd column. Literal '' in by=="project" mode is
	// deliberate (see AttributionGroup.Cwd's doc): a project-keyed group has
	// no single cwd to report, so we don't compute one instead of just not
	// having one.
	cwdExpr := "''"
	switch by {
	case "session":
		keyCol = "COALESCE(NULLIF(sess.parent_session_uuid, ''), sa.session_uuid)"
		cwdExpr = effCwdExpr
	case "cwd":
		// Three different things can land in this grouping and must stay
		// distinguishable on the wire: the unattributed sentinel
		// (sa.session_uuid == "", same key attribute.Unattributed uses
		// everywhere else - a fact about meter movement, not about any
		// session), UnknownCwd (a real, known session whose cwd was never
		// captured), and everything else (the effective owner's actual
		// cwd). Checking sa.session_uuid first, before falling back to
		// UnknownCwd, is what keeps the first two from colliding: without
		// it an unattributed row's (also empty) effCwdExpr would fall into
		// the UnknownCwd branch instead of keeping its own sentinel.
		keyCol = "CASE WHEN sa.session_uuid = '' THEN '' " +
			"WHEN " + effCwdExpr + " = '' THEN '" + UnknownCwd + "' " +
			"ELSE " + effCwdExpr + " END"
		cwdExpr = effCwdExpr
	}
	// MAX(sa.project) is a plain lookup, not an aggregate choice: a session
	// belongs to exactly one project, and grouping by project makes it the
	// key anyway. MAX(cwd) is the same kind of plain lookup in by=="session"
	// and by=="cwd" mode: every row folded into one effective-owner group
	// resolves cwdExpr to that same owner's single cwd, so MAX() just reads
	// it back rather than choosing among genuinely different values.
	//
	// peak is the largest share this group took in any single window, which
	// by=="project" and by=="cwd" get wrong if computed as a plain MAX()
	// over raw session_attribution rows: a project or directory pools many
	// distinct sessions, so that MAX() picks whichever single session was
	// biggest across the group's whole history, not the group's own worst
	// window (measured live: a project with exactly one window in range and
	// 116 sessions inside it reported peak_pct 9.4 against a pct of 37.1 -
	// one window in range means peak must equal the total by definition,
	// and 9.4 was just the largest session's own share). joinedPeaks below
	// sums every row sharing (key, window) first, then maxes across those
	// window sums instead of across raw rows.
	//
	// by=="session" keeps the plain MAX() over raw rows: it already keys on
	// the effective owner, one entry per (real) session_uuid contributing
	// to a window, so unlike project/cwd there's no widening effect on the
	// same measurement, only the longstanding case where a supervisor and
	// its own subagents land in the same window and aren't summed before
	// the max (documented on SessionPctTotalsAll's own FiveHPeakPct). Left
	// as-is here rather than folded into the same fix, so a consumer already
	// depending on this number sees it unchanged.
	peakExpr := "MAX(sa.measured_pct + sa.estimated_pct)"
	joinedPeaks := ""
	if by == "project" || by == "cwd" {
		peakExpr = "peaks.peak"
		joinedPeaks = `
			JOIN (
				SELECT k, MAX(window_pct) AS peak FROM (
					SELECT ` + keyCol + ` AS k,
					       sa.window_start_unix_ms AS window_start_unix_ms,
					       SUM(sa.measured_pct + sa.estimated_pct) AS window_pct
					FROM session_attribution sa
					LEFT JOIN sessions sess  ON sess.session_uuid = sa.session_uuid
					LEFT JOIN sessions psess ON psess.session_uuid = sess.parent_session_uuid
					WHERE sa.bucket = ? AND sa.window_start_unix_ms >= ?
					GROUP BY k, sa.window_start_unix_ms
				)
				GROUP BY k
			) peaks ON peaks.k = ` + keyCol + `
		`
	}
	q := `
		SELECT ` + keyCol + ` AS k,
		       MAX(sa.project)                      AS project,
		       MAX(` + cwdExpr + `)                 AS cwd,
		       SUM(sa.measured_pct + sa.estimated_pct) AS pct,
		       SUM(sa.measured_pct)                 AS measured,
		       SUM(sa.estimated_pct)                AS estimated,
		       ` + peakExpr + `                     AS peak,
		       SUM(sa.cw_tokens)                    AS cw,
		       SUM(sa.raw_tokens)                   AS raw,
		       SUM(sa.turn_count)                   AS turns,
		       COUNT(DISTINCT sa.session_uuid)      AS sessions,
		       COUNT(DISTINCT sa.window_start_unix_ms) AS windows,
		       MIN(NULLIF(sa.first_ts_unix_ms, 0))  AS first_ts,
		       MAX(sa.last_ts_unix_ms)              AS last_ts
		FROM session_attribution sa
		LEFT JOIN sessions sess  ON sess.session_uuid = sa.session_uuid
		LEFT JOIN sessions psess ON psess.session_uuid = sess.parent_session_uuid
		` + joinedPeaks + `
		WHERE sa.bucket = ? AND sa.window_start_unix_ms >= ?
		GROUP BY k
		ORDER BY pct DESC
	`
	args := []any{bucket, sinceMS}
	if joinedPeaks != "" {
		args = []any{bucket, sinceMS, bucket, sinceMS}
	}
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []AttributionGroup{}
	for rows.Next() {
		var (
			g, project, cwd sql.NullString
			firstTS, lastTS sql.NullInt64
			grp             AttributionGroup
		)
		if err := rows.Scan(&g, &project, &cwd, &grp.Pct, &grp.MeasuredPct, &grp.EstimatedPct, &grp.PeakPct,
			&grp.CWTokens, &grp.RawTokens, &grp.TurnCount,
			&grp.Sessions, &grp.Windows, &firstTS, &lastTS); err != nil {
			return nil, err
		}
		grp.Key = g.String
		grp.Project = project.String
		grp.Cwd = cwd.String
		grp.FirstTSUnixMS = firstTS.Int64
		grp.LastTSUnixMS = lastTS.Int64
		// The unattributed sentinel is a window property, not a session or a
		// project: COUNT(DISTINCT session_uuid) would otherwise credit it
		// with a session it doesn't have.
		if strings.TrimSpace(grp.Key) == "" {
			grp.Sessions = 0
		}
		out = append(out, grp)
	}
	return out, rows.Err()
}

// ListLimitWindows returns a bucket's windows starting at or after sinceMS,
// oldest first.
func (s *Store) ListLimitWindows(ctx context.Context, bucket string, sinceMS int64) ([]LimitWindowRow, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT start_unix_ms, end_unix_ms, reset_unix_ms,
		       inferred, partial, in_progress,
		       measured_pct, attributed_pct, peak_pct, hit_cap,
		       tokens_per_pct_cw
		FROM limit_windows
		WHERE bucket = ? AND start_unix_ms >= ?
		ORDER BY start_unix_ms ASC
	`, bucket, sinceMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []LimitWindowRow{}
	for rows.Next() {
		w := LimitWindowRow{Bucket: bucket}
		var inferred, partial, inProgress, hitCap int
		if err := rows.Scan(&w.StartUnixMS, &w.EndUnixMS, &w.ResetUnixMS,
			&inferred, &partial, &inProgress,
			&w.MeasuredPct, &w.AttributedPct, &w.PeakPct, &hitCap,
			&w.TokensPerPctCW); err != nil {
			return nil, err
		}
		w.Inferred, w.Partial, w.InProgress, w.HitCap = inferred == 1, partial == 1, inProgress == 1, hitCap == 1
		out = append(out, w)
	}
	return out, rows.Err()
}

// WindowSlices returns every session row for a bucket's windows starting at
// or after sinceMS, ordered oldest window first then largest share first:
// the shape the stacked per-window chart consumes.
//
// Each row also carries EffectiveSessionUUID (a LEFT JOIN to sessions, same
// COALESCE as GroupAttribution) so a caller grouping "by session" can fold a
// subagent's slice into its parent the way the group table beneath the
// chart already does, and Cwd (the same effective-owner resolution, via a
// second join to the parent's own sessions row) so a caller grouping
// "by cwd" can do the same keyed on directory instead. The row itself stays
// keyed by the actual spender (SessionUUID); nothing here changes what's
// stored, only what a consumer can key its own grouping on.
func (s *Store) WindowSlices(ctx context.Context, bucket string, sinceMS int64) ([]AttributionRow, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT sa.window_start_unix_ms, sa.session_uuid, sa.project,
		       sa.measured_pct, sa.estimated_pct,
		       sa.cw_tokens, sa.raw_tokens, sa.turn_count,
		       sa.first_ts_unix_ms, sa.last_ts_unix_ms,
		       COALESCE(NULLIF(sess.parent_session_uuid, ''), sa.session_uuid) AS effective_uuid,
		       COALESCE(CASE WHEN sess.parent_session_uuid <> '' THEN psess.cwd ELSE sess.cwd END, '') AS cwd
		FROM session_attribution sa
		LEFT JOIN sessions sess  ON sess.session_uuid = sa.session_uuid
		LEFT JOIN sessions psess ON psess.session_uuid = sess.parent_session_uuid
		WHERE sa.bucket = ? AND sa.window_start_unix_ms >= ?
		ORDER BY sa.window_start_unix_ms ASC, (sa.measured_pct + sa.estimated_pct) DESC
	`, bucket, sinceMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []AttributionRow{}
	for rows.Next() {
		r := AttributionRow{Bucket: bucket}
		if err := rows.Scan(&r.WindowStartUnixMS, &r.SessionUUID, &r.Project,
			&r.MeasuredPct, &r.EstimatedPct,
			&r.CWTokens, &r.RawTokens, &r.TurnCount,
			&r.FirstTSUnixMS, &r.LastTSUnixMS,
			&r.EffectiveSessionUUID, &r.Cwd); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ResolveSessionUUID expands a unique session-UUID prefix to the full UUID,
// so the CLI can take a short id the way git takes a short SHA. An exact
// 36-char match short-circuits. Returns an error naming the ambiguity when
// the prefix matches more than one session, so the caller can just surface
// it.
func (s *Store) ResolveSessionUUID(ctx context.Context, prefix string) (string, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return "", errors.New("empty session id")
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT session_uuid FROM sessions
		WHERE session_uuid = ? OR session_uuid LIKE ? || '%'
		ORDER BY last_ts_unix_ms DESC
		LIMIT 11
	`, prefix, prefix)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var matches []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return "", err
		}
		if u == prefix {
			return u, nil
		}
		matches = append(matches, u)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch {
	case len(matches) == 0:
		return "", fmt.Errorf("no session matches %q", prefix)
	case len(matches) == 1:
		return matches[0], nil
	default:
		shown := matches
		if len(shown) > 5 {
			shown = shown[:5]
		}
		return "", fmt.Errorf("%q is ambiguous: %d sessions match (%s…)",
			prefix, len(matches), strings.Join(shown, ", "))
	}
}
