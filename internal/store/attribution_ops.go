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
	rows, err := s.DB.QueryContext(ctx, `
		WITH per_window AS (
			SELECT
			    COALESCE(NULLIF(sess.parent_session_uuid, ''), sa.session_uuid) AS effective_uuid,
			    sa.project              AS project,
			    sa.bucket               AS bucket,
			    sa.window_start_unix_ms AS window_start_unix_ms,
			    SUM(sa.measured_pct)    AS measured,
			    SUM(sa.estimated_pct)   AS estimated
			FROM session_attribution sa
			LEFT JOIN sessions sess ON sess.session_uuid = sa.session_uuid
			WHERE sa.session_uuid <> ''
			GROUP BY effective_uuid, sa.bucket, sa.window_start_unix_ms
		)
		SELECT effective_uuid, MAX(project) AS project, bucket,
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
			uuid, project, bucket       string
			pct, peak, measured, estPct float64
			windows                     int
		)
		if err := rows.Scan(&uuid, &project, &bucket, &pct, &peak, &measured, &estPct, &windows); err != nil {
			return nil, err
		}
		t, ok := out[uuid]
		if !ok {
			t = &SessionPctTotals{SessionUUID: uuid, Project: project}
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
func (s *Store) SessionPctWindows(ctx context.Context, uuid, bucket string) ([]AttributionRow, []LimitWindowRow, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT a.window_start_unix_ms, a.project,
		       a.measured_pct, a.estimated_pct,
		       a.cw_tokens, a.raw_tokens, a.turn_count,
		       a.first_ts_unix_ms, a.last_ts_unix_ms,
		       w.end_unix_ms, w.inferred, w.partial, w.in_progress,
		       w.measured_pct, w.attributed_pct, w.peak_pct, w.hit_cap,
		       w.tokens_per_pct_cw
		FROM session_attribution a
		JOIN limit_windows w
		  ON w.bucket = a.bucket AND w.start_unix_ms = a.window_start_unix_ms
		WHERE a.session_uuid = ? AND a.bucket = ?
		ORDER BY a.window_start_unix_ms ASC
	`, uuid, bucket)
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
		if err := rows.Scan(
			&r.WindowStartUnixMS, &r.Project,
			&r.MeasuredPct, &r.EstimatedPct,
			&r.CWTokens, &r.RawTokens, &r.TurnCount,
			&r.FirstTSUnixMS, &r.LastTSUnixMS,
			&w.EndUnixMS, &inferred, &partial, &inProgress,
			&w.MeasuredPct, &w.AttributedPct, &w.PeakPct, &hitCap,
			&w.TokensPerPctCW,
		); err != nil {
			return nil, nil, err
		}
		w.StartUnixMS = r.WindowStartUnixMS
		w.Inferred, w.Partial, w.InProgress, w.HitCap = inferred == 1, partial == 1, inProgress == 1, hitCap == 1
		out = append(out, r)
		wins = append(wins, w)
	}
	return out, wins, rows.Err()
}

// AttributionGroup is a rollup over one grouping key (a project, or a
// session) within a time range.
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
}

// GroupAttribution rolls attribution up by project or by session over the
// windows that start at or after sinceMS. by is "project" or "session".
//
// by=="session" groups by the EFFECTIVE owner (COALESCE(NULLIF(
// parent_session_uuid, ""), session_uuid)) rather than the raw session_uuid,
// so a supervisor's row includes the subagents it dispatched instead of
// listing them as its peers. by=="project" is unaffected: a subagent already
// carries its parent's project (see internal/ingest), so grouping by project
// naturally already pools them, join or no join.
func (s *Store) GroupAttribution(ctx context.Context, bucket, by string, sinceMS int64) ([]AttributionGroup, error) {
	keyCol := "sa.project"
	if by == "session" {
		keyCol = "COALESCE(NULLIF(sess.parent_session_uuid, ''), sa.session_uuid)"
	}
	// MAX(sa.project) is a plain lookup, not an aggregate choice: a session
	// belongs to exactly one project, and grouping by project makes it the
	// key anyway.
	//
	// peak here is MAX() over raw session_attribution rows, same as the
	// project grouping already did before this change: two sessions (or a
	// supervisor and a subagent) sharing one window were already not summed
	// before taking the max, so this preserves that existing approximation
	// rather than introducing a new one just for the session grouping.
	q := `
		SELECT ` + keyCol + ` AS k,
		       MAX(sa.project)                      AS project,
		       SUM(sa.measured_pct + sa.estimated_pct) AS pct,
		       SUM(sa.measured_pct)                 AS measured,
		       SUM(sa.estimated_pct)                AS estimated,
		       MAX(sa.measured_pct + sa.estimated_pct) AS peak,
		       SUM(sa.cw_tokens)                    AS cw,
		       SUM(sa.raw_tokens)                   AS raw,
		       SUM(sa.turn_count)                   AS turns,
		       COUNT(DISTINCT sa.session_uuid)      AS sessions,
		       COUNT(DISTINCT sa.window_start_unix_ms) AS windows,
		       MIN(NULLIF(sa.first_ts_unix_ms, 0))  AS first_ts,
		       MAX(sa.last_ts_unix_ms)              AS last_ts
		FROM session_attribution sa
		LEFT JOIN sessions sess ON sess.session_uuid = sa.session_uuid
		WHERE sa.bucket = ? AND sa.window_start_unix_ms >= ?
		GROUP BY k
		ORDER BY pct DESC
	`
	rows, err := s.DB.QueryContext(ctx, q, bucket, sinceMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []AttributionGroup{}
	for rows.Next() {
		var (
			g, project      sql.NullString
			firstTS, lastTS sql.NullInt64
			grp             AttributionGroup
		)
		if err := rows.Scan(&g, &project, &grp.Pct, &grp.MeasuredPct, &grp.EstimatedPct, &grp.PeakPct,
			&grp.CWTokens, &grp.RawTokens, &grp.TurnCount,
			&grp.Sessions, &grp.Windows, &firstTS, &lastTS); err != nil {
			return nil, err
		}
		grp.Key = g.String
		grp.Project = project.String
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
func (s *Store) WindowSlices(ctx context.Context, bucket string, sinceMS int64) ([]AttributionRow, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT window_start_unix_ms, session_uuid, project,
		       measured_pct, estimated_pct,
		       cw_tokens, raw_tokens, turn_count,
		       first_ts_unix_ms, last_ts_unix_ms
		FROM session_attribution
		WHERE bucket = ? AND window_start_unix_ms >= ?
		ORDER BY window_start_unix_ms ASC, (measured_pct + estimated_pct) DESC
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
			&r.FirstTSUnixMS, &r.LastTSUnixMS); err != nil {
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
