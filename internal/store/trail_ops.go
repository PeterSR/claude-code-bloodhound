package store

import (
	"context"
	"database/sql"
	"errors"
)

// --- Row types -------------------------------------------------------

// TrailWatermark tracks how far Trail has analysed a session. Exists
// from first sight (before any brief): analyzed_through is set to the
// session's latest record ts and nothing is analysed that round.
type TrailWatermark struct {
	SessionUUID           string
	FirstSeenUnixMS       int64
	AnalyzedThroughUnixMS int64
}

// TrailBrief is the current per-session summary.
type TrailBrief struct {
	SessionUUID   string
	Project       string
	Cwd           string
	Headline      string
	Summary       string
	UpdatedUnixMS int64
	AnalyzedRuns  int
}

// TrailRepo is one normalized session<->worktree edge. RepoPath is the
// absolute worktree path (the identity); display dirname + live branch
// are derived from it at render time. BranchCached is the last-known
// branch, a fallback for when the worktree has moved/gone.
type TrailRepo struct {
	SessionUUID  string
	RepoPath     string
	Role         string // primary | incidental
	BranchCached string
}

// TrailLoop is one outstanding item. Status is the analyzer's view;
// UserStatus (when non-nil) is a sticky manual override. Effective
// status is UserStatus if set, else Status.
type TrailLoop struct {
	SessionUUID       string
	LoopKey           string
	Text              string
	Status            string
	RelatedRepoPath   string
	FirstSeenUnixMS   int64
	LastSeenUnixMS    int64
	UserStatus        *string
	UserNote          string
	UserUpdatedUnixMS *int64
}

// EffectiveStatus is UserStatus when the user has overridden, else the
// analyzer's Status.
func (l TrailLoop) EffectiveStatus() string {
	if l.UserStatus != nil && *l.UserStatus != "" {
		return *l.UserStatus
	}
	return l.Status
}

// TrailRun is one analyzer invocation. cost_* are filled directly
// (headless envelope or by summing the analyzer's own JSONL tokens).
type TrailRun struct {
	ID                int64
	TrailSessionUUID  string
	TargetSessionUUID string
	Mode              string
	StartedUnixMS     int64
	FinishedUnixMS    *int64
	OK                bool
	Error             string
	CostUSD           float64
	InputTokens       int64
	OutputTokens      int64
	CacheReadTokens   int64
	CacheCreateTokens int64
}

// TrailAnalysis is the full result of one session analysis, persisted
// atomically by SaveTrailAnalysis.
type TrailAnalysis struct {
	Brief Brief
	// AnalyzedThroughUnixMS advances the watermark to the last record ts
	// that this analysis folded in.
	AnalyzedThroughUnixMS int64
}

// Brief is the analysis payload for one session (brief + repos + loops).
type Brief struct {
	SessionUUID string
	Project     string
	Cwd         string
	Headline    string
	Summary     string
	Repos       []TrailRepo
	Loops       []BriefLoop
}

// BriefLoop is one loop as reported by the analyzer (no user-override
// fields — those are owned by the store and preserved across runs).
type BriefLoop struct {
	LoopKey         string
	Text            string
	Status          string
	RelatedRepoPath string
}

// --- Watermark -------------------------------------------------------

// GetTrailWatermark returns the watermark for a session, or nil if Trail
// has never seen it.
func (s *Store) GetTrailWatermark(ctx context.Context, sessionUUID string) (*TrailWatermark, error) {
	var w TrailWatermark
	err := s.DB.QueryRowContext(ctx, `
		SELECT session_uuid, first_seen_unix_ms, analyzed_through_unix_ms
		FROM trail_watermarks WHERE session_uuid = ?
	`, sessionUUID).Scan(&w.SessionUUID, &w.FirstSeenUnixMS, &w.AnalyzedThroughUnixMS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// SetTrailWatermark upserts a watermark. Used both on first sight (no
// brief produced) and to advance after an analysis.
func (s *Store) SetTrailWatermark(ctx context.Context, w TrailWatermark) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO trail_watermarks (session_uuid, first_seen_unix_ms, analyzed_through_unix_ms)
		VALUES (?,?,?)
		ON CONFLICT(session_uuid) DO UPDATE SET
			analyzed_through_unix_ms = excluded.analyzed_through_unix_ms
	`, w.SessionUUID, w.FirstSeenUnixMS, w.AnalyzedThroughUnixMS)
	return err
}

// --- Analysis persistence -------------------------------------------

// SaveTrailAnalysis persists one session's analysis atomically: upsert
// the brief, replace its repo edges, upsert its loops (preserving
// first_seen + any user override, staling loops the analyzer dropped),
// and advance the watermark.
func (s *Store) SaveTrailAnalysis(ctx context.Context, a TrailAnalysis, runMS int64) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	b := a.Brief

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO trail_briefs
			(session_uuid, project, cwd, headline, summary, updated_unix_ms, analyzed_runs)
		VALUES (?,?,?,?,?,?,1)
		ON CONFLICT(session_uuid) DO UPDATE SET
			project        = excluded.project,
			cwd            = excluded.cwd,
			headline       = excluded.headline,
			summary        = excluded.summary,
			updated_unix_ms= excluded.updated_unix_ms,
			analyzed_runs  = trail_briefs.analyzed_runs + 1
	`, b.SessionUUID, b.Project, b.Cwd, b.Headline, b.Summary, runMS); err != nil {
		return err
	}

	// Repos: replace-all for this session.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM trail_brief_repos WHERE session_uuid = ?`, b.SessionUUID); err != nil {
		return err
	}
	if len(b.Repos) > 0 {
		rs, err := tx.PrepareContext(ctx, `
			INSERT OR REPLACE INTO trail_brief_repos (session_uuid, repo_path, role, branch_cached)
			VALUES (?,?,?,?)
		`)
		if err != nil {
			return err
		}
		defer rs.Close()
		for _, r := range b.Repos {
			role := r.Role
			if role == "" {
				role = "primary"
			}
			if _, err := rs.ExecContext(ctx, b.SessionUUID, r.RepoPath, role, r.BranchCached); err != nil {
				return err
			}
		}
	}

	// Loops: upsert each reported loop (preserving first_seen + user
	// override fields via the conflict clause), then stale any
	// non-user-resolved loop the analyzer didn't re-report this run.
	if len(b.Loops) > 0 {
		ls, err := tx.PrepareContext(ctx, `
			INSERT INTO trail_open_loops
				(session_uuid, loop_key, text, status, related_repo_path,
				 first_seen_unix_ms, last_seen_unix_ms)
			VALUES (?,?,?,?,?,?,?)
			ON CONFLICT(session_uuid, loop_key) DO UPDATE SET
				text              = excluded.text,
				status            = excluded.status,
				related_repo_path = excluded.related_repo_path,
				last_seen_unix_ms = excluded.last_seen_unix_ms
		`)
		if err != nil {
			return err
		}
		defer ls.Close()
		for _, lp := range b.Loops {
			if _, err := ls.ExecContext(ctx,
				b.SessionUUID, lp.LoopKey, lp.Text, lp.Status, lp.RelatedRepoPath,
				runMS, runMS,
			); err != nil {
				return err
			}
		}
	}
	// Stale loops the analyzer dropped this run — but never touch
	// user-resolved ones (their effective status is owned by the user).
	if _, err := tx.ExecContext(ctx, `
		UPDATE trail_open_loops
		SET status = 'stale'
		WHERE session_uuid = ?
		  AND user_status IS NULL
		  AND last_seen_unix_ms < ?
		  AND status != 'stale'
	`, b.SessionUUID, runMS); err != nil {
		return err
	}

	// Advance watermark.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO trail_watermarks (session_uuid, first_seen_unix_ms, analyzed_through_unix_ms)
		VALUES (?,?,?)
		ON CONFLICT(session_uuid) DO UPDATE SET
			analyzed_through_unix_ms = excluded.analyzed_through_unix_ms
	`, b.SessionUUID, runMS, a.AnalyzedThroughUnixMS); err != nil {
		return err
	}

	return tx.Commit()
}

// ResolveTrailLoop sets (or clears) the manual override on a loop. Pass
// userStatus "" to clear it (re-hand control to the analyzer).
func (s *Store) ResolveTrailLoop(ctx context.Context, sessionUUID, loopKey, userStatus, note string, atMS int64) error {
	var us any
	var at any
	if userStatus != "" {
		us = userStatus
		at = atMS
	}
	_, err := s.DB.ExecContext(ctx, `
		UPDATE trail_open_loops
		SET user_status = ?, user_note = ?, user_updated_unix_ms = ?
		WHERE session_uuid = ? AND loop_key = ?
	`, us, note, at, sessionUUID, loopKey)
	return err
}

// --- Trail runs (cost attribution) ----------------------------------

// StartTrailRun records a launching analyzer invocation and returns its
// row id. Written BEFORE the analyzer produces any JSONL so the
// ingester's skip-set already contains trail_session_uuid.
func (s *Store) StartTrailRun(ctx context.Context, r TrailRun) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `
		INSERT INTO trail_runs
			(trail_session_uuid, target_session_uuid, mode, started_unix_ms)
		VALUES (?,?,?,?)
	`, r.TrailSessionUUID, r.TargetSessionUUID, r.Mode, r.StartedUnixMS)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishTrailRun records the outcome + cost of a completed run.
func (s *Store) FinishTrailRun(ctx context.Context, r TrailRun) error {
	okI := 0
	if r.OK {
		okI = 1
	}
	_, err := s.DB.ExecContext(ctx, `
		UPDATE trail_runs SET
			finished_unix_ms=?, ok=?, error=?,
			cost_usd=?, input_tokens=?, output_tokens=?,
			cache_read_tokens=?, cache_create_tokens=?
		WHERE id=?
	`, r.FinishedUnixMS, okI, r.Error,
		r.CostUSD, r.InputTokens, r.OutputTokens,
		r.CacheReadTokens, r.CacheCreateTokens, r.ID)
	return err
}

// TrailSessionUUIDs returns the set of analyzer session UUIDs, for the
// ingester to skip (so analyzer turns never enter the turns table).
func (s *Store) TrailSessionUUIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT DISTINCT trail_session_uuid FROM trail_runs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return out, err
		}
		out[u] = true
	}
	return out, rows.Err()
}

// TrailCostTotals is the aggregate cost of all Trail analyzer runs.
type TrailCostTotals struct {
	Runs              int64
	CostUSD           float64
	InputTokens       int64
	OutputTokens      int64
	CacheReadTokens   int64
	CacheCreateTokens int64
}

// TrailCost sums the cost across all analyzer runs.
func (s *Store) TrailCost(ctx context.Context) (TrailCostTotals, error) {
	var t TrailCostTotals
	err := s.DB.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(cost_usd),0),
		       COALESCE(SUM(input_tokens),0),
		       COALESCE(SUM(output_tokens),0),
		       COALESCE(SUM(cache_read_tokens),0),
		       COALESCE(SUM(cache_create_tokens),0)
		FROM trail_runs
	`).Scan(&t.Runs, &t.CostUSD, &t.InputTokens, &t.OutputTokens,
		&t.CacheReadTokens, &t.CacheCreateTokens)
	return t, err
}

// --- Read paths (for the API) ---------------------------------------

// ListTrailBriefs returns all current briefs, most-recently-updated
// first.
func (s *Store) ListTrailBriefs(ctx context.Context) ([]TrailBrief, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT session_uuid, project, cwd, headline, summary, updated_unix_ms, analyzed_runs
		FROM trail_briefs ORDER BY updated_unix_ms DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrailBrief
	for rows.Next() {
		var b TrailBrief
		if err := rows.Scan(&b.SessionUUID, &b.Project, &b.Cwd, &b.Headline,
			&b.Summary, &b.UpdatedUnixMS, &b.AnalyzedRuns); err != nil {
			return out, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ListTrailRepos returns all session<->repo edges.
func (s *Store) ListTrailRepos(ctx context.Context) ([]TrailRepo, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT session_uuid, repo_path, role, branch_cached FROM trail_brief_repos ORDER BY repo_path, role
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrailRepo
	for rows.Next() {
		var r TrailRepo
		if err := rows.Scan(&r.SessionUUID, &r.RepoPath, &r.Role, &r.BranchCached); err != nil {
			return out, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListTrailLoops returns loops. When includeResolved is false, loops
// whose effective status is done/dismissed/stale are omitted.
func (s *Store) ListTrailLoops(ctx context.Context, includeResolved bool) ([]TrailLoop, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT session_uuid, loop_key, text, status, related_repo_path,
		       first_seen_unix_ms, last_seen_unix_ms,
		       user_status, user_note, user_updated_unix_ms
		FROM trail_open_loops
		ORDER BY last_seen_unix_ms DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrailLoop
	for rows.Next() {
		var l TrailLoop
		var us sql.NullString
		var uu sql.NullInt64
		if err := rows.Scan(&l.SessionUUID, &l.LoopKey, &l.Text, &l.Status, &l.RelatedRepoPath,
			&l.FirstSeenUnixMS, &l.LastSeenUnixMS, &us, &l.UserNote, &uu); err != nil {
			return out, err
		}
		if us.Valid {
			l.UserStatus = &us.String
		}
		if uu.Valid {
			l.UserUpdatedUnixMS = &uu.Int64
		}
		if !includeResolved {
			switch l.EffectiveStatus() {
			case "done", "dismissed", "stale":
				continue
			}
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
