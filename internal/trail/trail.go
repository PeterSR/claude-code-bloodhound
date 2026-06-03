// Package trail is bloodhound's passive work-tracker: each cycle it
// reads the recent activity of recently-active Claude Code sessions and
// keeps a per-session brief (what's in flight, which worktrees it
// touches, outstanding open loops). It drives `claude` to summarise,
// reusing the self-heal launch machinery, and attributes its own cost
// via trail_runs (its analyzer sessions are skipped by the ingester).
package trail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/config"
	"github.com/PeterSR/claude-code-bloodhound/internal/gitmeta"
	"github.com/PeterSR/claude-code-bloodhound/internal/sessioninsight"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
	"github.com/PeterSR/claude-code-bloodhound/internal/usage/selfheal"
)

// maxSessionsPerCycle caps how many sessions one cycle analyses, so a
// burst of activity can't fan out into an unbounded number of analyzer
// runs (and cost).
const maxSessionsPerCycle = 12

// Stats summarises one Run.
type Stats struct {
	Considered int
	FirstSeen  int // sessions seen for the first time (watermark set, no analysis)
	Analyzed   int
	Skipped    int // active but no new activity past the watermark
	Errors     []string
	CostUSD    float64
	ElapsedS   float64
}

// Run executes one Trail cycle.
func Run(ctx context.Context, s *store.Store, cfg config.Config, w io.Writer) (Stats, error) {
	t0 := time.Now()
	var st Stats

	window := cfg.TrailWindowS
	if window <= 0 {
		window = cfg.ActiveSessionThresholdS
	}
	if window <= 0 {
		window = 1800
	}

	projectsDir, err := config.ClaudeProjectsDir()
	if err != nil {
		return st, err
	}
	stateDir, _ := config.StateDir()

	refs, err := sessioninsight.RecentN(ctx, s.DB, t0, window, maxSessionsPerCycle)
	if err != nil {
		return st, err
	}
	st.Considered = len(refs)

	az := claudeAnalyzer{
		ClaudeBinary: cfg.ClaudeBinary,
		Mode:         cfg.TrailMode,
		Cwd:          stateDir, // pinned (shares the daemon's trusted dir)
		ProjectsDir:  projectsDir,
		Timeout:      120 * time.Second,
	}

	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		path := LocateSessionJSONL(projectsDir, ref.UUID)
		if path == "" {
			st.Skipped++
			continue
		}

		wm, err := s.GetTrailWatermark(ctx, ref.UUID)
		if err != nil {
			st.Errors = append(st.Errors, ref.UUID+": watermark: "+err.Error())
			continue
		}
		if wm == nil {
			// First sight: record how far we've seen, analyse nothing.
			_, maxTS, _, rerr := ReadSince(path, 1<<62)
			if rerr != nil {
				st.Errors = append(st.Errors, ref.UUID+": scan: "+rerr.Error())
				continue
			}
			_ = s.SetTrailWatermark(ctx, store.TrailWatermark{
				SessionUUID:           ref.UUID,
				FirstSeenUnixMS:       t0.UnixMilli(),
				AnalyzedThroughUnixMS: maxTS,
			})
			st.FirstSeen++
			continue
		}

		recs, maxTS, cwd, rerr := ReadSince(path, wm.AnalyzedThroughUnixMS)
		if rerr != nil {
			st.Errors = append(st.Errors, ref.UUID+": read: "+rerr.Error())
			continue
		}
		if len(recs) == 0 {
			st.Skipped++
			continue
		}
		if cwd == "" {
			cwd = ref.Project
		}

		prior, _ := s.GetTrailBrief(ctx, ref.UUID)
		priorLoops, _ := s.TrailLoopsForSession(ctx, ref.UUID)

		sessionID := selfheal.NewSessionID()
		runID, _ := s.StartTrailRun(ctx, store.TrailRun{
			TrailSessionUUID:  sessionID,
			TargetSessionUUID: ref.UUID,
			Mode:              az.Mode,
			StartedUnixMS:     t0.UnixMilli(),
		})

		res, aerr := az.Analyze(ctx, Input{
			SessionUUID:       ref.UUID,
			Project:           ref.Project,
			Cwd:               cwd,
			PriorBrief:        prior,
			PriorLoops:        priorLoops,
			Records:           recs,
			AnalyzerSessionID: sessionID,
		})
		fin := time.Now().UnixMilli()
		if aerr != nil {
			st.Errors = append(st.Errors, ref.UUID+": analyze: "+aerr.Error())
			_ = s.FinishTrailRun(ctx, store.TrailRun{ID: runID, FinishedUnixMS: &fin, OK: false, Error: aerr.Error(),
				CostUSD: res.Cost.CostUSD, InputTokens: res.Cost.InputTokens, OutputTokens: res.Cost.OutputTokens,
				CacheReadTokens: res.Cost.CacheReadTokens, CacheCreateTokens: res.Cost.CacheCreateTokens})
			continue
		}

		brief := buildBrief(ref, cwd, res.Args)
		if err := s.SaveTrailAnalysis(ctx, store.TrailAnalysis{Brief: brief, AnalyzedThroughUnixMS: maxTS}, fin); err != nil {
			st.Errors = append(st.Errors, ref.UUID+": save: "+err.Error())
		}
		_ = s.FinishTrailRun(ctx, store.TrailRun{ID: runID, FinishedUnixMS: &fin, OK: true,
			CostUSD: res.Cost.CostUSD, InputTokens: res.Cost.InputTokens, OutputTokens: res.Cost.OutputTokens,
			CacheReadTokens: res.Cost.CacheReadTokens, CacheCreateTokens: res.Cost.CacheCreateTokens})
		st.Analyzed++
		st.CostUSD += res.Cost.CostUSD
	}

	st.ElapsedS = time.Since(t0).Seconds()
	return st, nil
}

// buildBrief converts the analyzer's args into a store.Brief, resolving
// the primary worktree from the session cwd (live git) and enriching
// each repo with its live branch (cached as a fallback).
func buildBrief(ref sessioninsight.SessionRef, cwd string, args SaveBriefArgs) store.Brief {
	seen := map[string]bool{}
	var repos []store.TrailRepo

	addRepo := func(path, role string) {
		if path == "" {
			return
		}
		g := gitmeta.Look(path, "")
		rp := g.Root
		if rp == "" {
			rp = path
		}
		if seen[rp] {
			return
		}
		seen[rp] = true
		repos = append(repos, store.TrailRepo{RepoPath: rp, Role: role, BranchCached: g.Branch})
	}

	// Primary = the session's own cwd worktree.
	addRepo(cwd, "primary")
	for _, r := range args.Repos {
		role := r.Role
		if role == "" || role == "primary" {
			// Only the cwd is primary; anything else the analyzer named
			// is incidental.
			if r.Path != cwd {
				role = "incidental"
			} else {
				role = "primary"
			}
		}
		addRepo(r.Path, role)
	}

	var loops []store.BriefLoop
	for _, l := range args.OpenLoops {
		key := l.Key
		if key == "" {
			key = autoLoopKey(l.Text)
		}
		loops = append(loops, store.BriefLoop{
			LoopKey:         key,
			Text:            l.Text,
			Status:          normalizeStatus(l.Status),
			RelatedRepoPath: l.RelatedRepoPath,
		})
	}

	return store.Brief{
		SessionUUID: ref.UUID,
		Project:     ref.Project,
		Cwd:         cwd,
		Headline:    args.Headline,
		Summary:     args.Summary,
		Repos:       repos,
		Loops:       loops,
	}
}

func normalizeStatus(s string) string {
	switch s {
	case "active", "blocked", "waiting", "done":
		return s
	default:
		return "active"
	}
}

// autoLoopKey is a content-derived fallback key for when the analyzer
// omits one — stable across runs for the same text.
func autoLoopKey(text string) string {
	h := sha256.Sum256([]byte(text))
	return "auto-" + hex.EncodeToString(h[:4])
}
