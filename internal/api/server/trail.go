package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
	"github.com/PeterSR/claude-code-bloodhound/internal/config"
	"github.com/PeterSR/claude-code-bloodhound/internal/gitmeta"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

func (s *Server) handleTrail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()

	// Initialise slices so empties serialise as [] not null — a null
	// would crash array access in the client.
	out := routes.TrailResponse{
		ServerNowMS: now.UnixMilli(),
		Sessions:    []routes.TrailSession{},
		Repos:       []routes.TrailRepoGroup{},
	}
	// Briefs older than the recent-session window drop off the page —
	// Trail is an overview of what's IN FLIGHT, not an archive. The rows
	// stay in the store; tighten/loosen via recent_session_window_s.
	recentWindowS := 86400
	if cfg, err := config.Load(); err == nil {
		out.Enabled = cfg.TrailEnabled
		out.Mode = cfg.TrailMode
		out.IntervalS = cfg.TrailIntervalS
		if cfg.RecentSessionWindowS > 0 {
			recentWindowS = cfg.RecentSessionWindowS
		}
	}
	cutoffMS := now.UnixMilli() - int64(recentWindowS)*1000

	allBriefs, err := s.Store.ListTrailBriefs(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	briefs := allBriefs[:0]
	for _, b := range allBriefs {
		if b.UpdatedUnixMS >= cutoffMS {
			briefs = append(briefs, b)
		}
	}
	repos, _ := s.Store.ListTrailRepos(ctx)
	loops, _ := s.Store.ListTrailLoops(ctx, false) // open loops only (effective)

	// Resolve live git facts once per distinct worktree path.
	type live struct{ dirname, branch, commonDir string }
	liveCache := map[string]live{}
	resolve := func(path, cached string) live {
		if v, ok := liveCache[path]; ok {
			return v
		}
		g := gitmeta.Look(path, cached)
		v := live{dirname: g.Dirname, branch: g.Branch, commonDir: g.CommonDir}
		liveCache[path] = v
		return v
	}

	// Index repos + loops by session.
	reposBySession := map[string][]store.TrailRepo{}
	for _, rp := range repos {
		reposBySession[rp.SessionUUID] = append(reposBySession[rp.SessionUUID], rp)
	}
	loopsBySession := map[string][]store.TrailLoop{}
	for _, l := range loops {
		loopsBySession[l.SessionUUID] = append(loopsBySession[l.SessionUUID], l)
	}

	headlineBySession := map[string]string{}
	for _, b := range briefs {
		headlineBySession[b.SessionUUID] = b.Headline
	}

	// By-session view.
	for _, b := range briefs {
		ts := routes.TrailSession{
			SessionUUID:   b.SessionUUID,
			Project:       b.Project,
			Cwd:           b.Cwd,
			Headline:      b.Headline,
			Summary:       b.Summary,
			UpdatedUnixMS: b.UpdatedUnixMS,
			AnalyzedRuns:  b.AnalyzedRuns,
			Repos:         []routes.TrailRepoRef{},
			OpenLoops:     []routes.TrailLoop{},
		}
		for _, rp := range reposBySession[b.SessionUUID] {
			lv := resolve(rp.RepoPath, rp.BranchCached)
			ts.Repos = append(ts.Repos, routes.TrailRepoRef{
				Path:    rp.RepoPath,
				Dirname: lv.dirname,
				Branch:  lv.branch,
				Role:    rp.Role,
			})
		}
		for _, l := range loopsBySession[b.SessionUUID] {
			ts.OpenLoops = append(ts.OpenLoops, toRouteLoop(l))
		}
		out.Sessions = append(out.Sessions, ts)
	}

	// By-repo pivot, three levels: repo (git common-dir; non-repo paths
	// stand alone) → worktree folder (path + live branch) → sessions.
	visible := map[string]bool{}
	for _, b := range briefs {
		visible[b.SessionUUID] = true
	}
	type wt struct {
		ref  routes.TrailWorktree
		seen map[string]bool
	}
	type grp struct {
		ref     routes.TrailRepoGroup
		wts     map[string]*wt
		wtOrder []string
	}
	groups := map[string]*grp{}
	var order []string
	for _, rp := range repos {
		if !visible[rp.SessionUUID] {
			continue
		}
		lv := resolve(rp.RepoPath, rp.BranchCached)
		key := lv.commonDir
		isRepo := key != ""
		if key == "" {
			key = rp.RepoPath
		}
		g := groups[key]
		if g == nil {
			g = &grp{
				ref: routes.TrailRepoGroup{
					Key:     key,
					Dirname: repoDisplayName(key, lv.dirname, isRepo),
					IsRepo:  isRepo,
				},
				wts: map[string]*wt{},
			}
			groups[key] = g
			order = append(order, key)
		}
		t := g.wts[rp.RepoPath]
		if t == nil {
			t = &wt{
				ref: routes.TrailWorktree{
					Path:    rp.RepoPath,
					Dirname: lv.dirname,
					Branch:  lv.branch,
				},
				seen: map[string]bool{},
			}
			g.wts[rp.RepoPath] = t
			g.wtOrder = append(g.wtOrder, rp.RepoPath)
		}
		if !t.seen[rp.SessionUUID] {
			t.seen[rp.SessionUUID] = true
			t.ref.Sessions = append(t.ref.Sessions, routes.TrailRepoSessionRef{
				SessionUUID: rp.SessionUUID,
				Headline:    headlineBySession[rp.SessionUUID],
				Role:        rp.Role,
			})
		}
	}
	for _, k := range order {
		g := groups[k]
		for _, wk := range g.wtOrder {
			g.ref.Worktrees = append(g.ref.Worktrees, g.wts[wk].ref)
		}
		out.Repos = append(out.Repos, g.ref)
	}

	// Cost.
	if c, err := s.Store.TrailCost(ctx); err == nil {
		cw := float64(c.InputTokens) + float64(c.OutputTokens)*5.0 +
			float64(c.CacheReadTokens)*0.1 + float64(c.CacheCreateTokens)*1.5
		out.Cost = routes.TrailCost{
			Runs:               c.Runs,
			CostUSD:            round2(c.CostUSD),
			InputTokens:        c.InputTokens,
			OutputTokens:       c.OutputTokens,
			CacheReadTokens:    c.CacheReadTokens,
			CacheCreateTokens:  c.CacheCreateTokens,
			CostWeightedTokens: round2(cw),
		}
		if median, _, n, ok, _ := s.Store.LatestCalibrationMedian(ctx, "week", 10); ok && n > 0 && median > 0 {
			out.Cost.PctOfWeek = round2(cw / median)
		}
	}

	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleTrailResolve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	var req routes.TrailResolveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if req.SessionUUID == "" || req.LoopKey == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "session_uuid and loop_key are required"})
		return
	}
	if err := s.Store.ResolveTrailLoop(r.Context(), req.SessionUUID, req.LoopKey, req.UserStatus, req.Note, time.Now().UnixMilli()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// repoDisplayName derives the repo-level label from the grouping key.
// For git repos the key is the common dir (usually <main-worktree>/.git,
// or a bare <name>.git); for non-repo paths it's the path itself, so the
// worktree dirname doubles as the label.
func repoDisplayName(key, dirname string, isRepo bool) string {
	if !isRepo {
		return dirname
	}
	base := filepath.Base(key)
	if base == ".git" {
		return filepath.Base(filepath.Dir(key))
	}
	return strings.TrimSuffix(base, ".git")
}

func toRouteLoop(l store.TrailLoop) routes.TrailLoop {
	rl := routes.TrailLoop{
		SessionUUID:     l.SessionUUID,
		Key:             l.LoopKey,
		Text:            l.Text,
		Status:          l.EffectiveStatus(),
		AnalyzerStatus:  l.Status,
		UserNote:        l.UserNote,
		RelatedRepoPath: l.RelatedRepoPath,
		FirstSeenUnixMS: l.FirstSeenUnixMS,
		LastSeenUnixMS:  l.LastSeenUnixMS,
	}
	if l.UserStatus != nil {
		rl.UserStatus = *l.UserStatus
	}
	return rl
}
