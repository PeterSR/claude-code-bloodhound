package server

import (
	"encoding/json"
	"net/http"
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
	if cfg, err := config.Load(); err == nil {
		out.Enabled = cfg.TrailEnabled
		out.Mode = cfg.TrailMode
		out.IntervalS = cfg.TrailIntervalS
	}

	briefs, err := s.Store.ListTrailBriefs(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
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

	// By-repo pivot. Group by live common-dir when available (so two
	// worktrees of one repo collapse), else by path.
	type grp struct {
		ref      routes.TrailRepoGroup
		sessions []routes.TrailRepoSessionRef
		seen     map[string]bool
	}
	groups := map[string]*grp{}
	var order []string
	for _, rp := range repos {
		lv := resolve(rp.RepoPath, rp.BranchCached)
		key := lv.commonDir
		if key == "" {
			key = rp.RepoPath
		}
		g := groups[key]
		if g == nil {
			g = &grp{
				ref: routes.TrailRepoGroup{
					Path:      rp.RepoPath,
					Dirname:   lv.dirname,
					Branch:    lv.branch,
					CommonDir: lv.commonDir,
				},
				seen: map[string]bool{},
			}
			groups[key] = g
			order = append(order, key)
		}
		if !g.seen[rp.SessionUUID] {
			g.seen[rp.SessionUUID] = true
			g.sessions = append(g.sessions, routes.TrailRepoSessionRef{
				SessionUUID: rp.SessionUUID,
				Headline:    headlineBySession[rp.SessionUUID],
				Role:        rp.Role,
			})
		}
	}
	for _, k := range order {
		g := groups[k]
		g.ref.Sessions = g.sessions
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
