package server

import (
	"net/http"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
	"github.com/PeterSR/claude-code-bloodhound/internal/config"
	"github.com/PeterSR/claude-code-bloodhound/internal/usage"
	"github.com/PeterSR/claude-code-bloodhound/internal/version"
)

func (s *Server) handleDebug(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()

	dataDir, _ := config.DataDir()
	stateDir, _ := config.StateDir()
	cfgDir, _ := config.ConfigDir()
	cfgFile, _ := config.Path()
	projects, _ := config.ClaudeProjectsDir()
	extractorPath, snapPath, _ := usage.ExtractorPaths()

	device, _ := s.Store.DeviceID(ctx)
	schema, _ := s.Store.SchemaVersion(ctx)

	res := routes.DebugResponse{
		Build: routes.DebugBuildInfo{
			Version: version.Version,
			Commit:  version.Commit,
			Date:    version.Date,
		},
		Paths: routes.DebugPathsInfo{
			DataDir:        dataDir,
			StateDir:       stateDir,
			ConfigDir:      cfgDir,
			ConfigFile:     cfgFile,
			ClaudeProjects: projects,
			ExtractorFile:  extractorPath,
			ExtractorSnap:  snapPath,
			DBPath:         s.Store.Path,
		},
		DeviceID:  device,
		Schema:    schema,
		ServerNow: now.UTC().Format(time.RFC3339),
	}

	if ext, origin, err := usage.LoadExtractor(); err == nil {
		res.Extractor = routes.DebugExtractorInfo{
			Origin:      string(origin),
			Version:     ext.Version,
			GeneratedAt: ext.GeneratedAt,
			GeneratedBy: ext.GeneratedBy,
			FieldCount:  len(ext.Fields),
		}
	}

	if stats, err := s.Store.IngestStats(ctx); err == nil {
		res.Ingest = routes.DebugIngestInfo{
			FilesTracked:     stats.FilesTracked,
			TurnsTotal:       stats.TurnsTotal,
			CompactionsTotal: stats.CompactionsTotal,
		}
		if stats.LatestTurnTSMS > 0 {
			res.Ingest.LatestTurnTS = time.UnixMilli(stats.LatestTurnTSMS).UTC().Format(time.RFC3339)
		}
	}

	acct, ok := s.withAccount(w, r)
	if !ok {
		return
	}
	if obs, err := s.Store.LatestUsage(ctx, acct); err == nil && obs != nil {
		res.LastPoll = &routes.DebugLastPollInfo{
			TS:              obs.TSISO,
			AgeS:            (now.UnixMilli() - obs.TSUnixMS) / 1000,
			ParseOK:         obs.ParseOK,
			ElapsedS:        obs.ElapsedS,
			SessionPct:      obs.SessionPct,
			WeekPct:         obs.WeekPct,
			SessionResetRaw: obs.SessionResetRaw,
			WeekResetRaw:    obs.WeekResetRaw,
		}
	}

	if dump, ts, err := s.Store.LatestRawDump(ctx); err == nil && dump != "" {
		res.RawDump = &routes.DebugRawDumpInfo{
			TS:    time.UnixMilli(ts).UTC().Format(time.RFC3339),
			Tail:  dump,
			Bytes: len(dump),
		}
	}

	// Aggregate summary: count from sessions and buckets.
	var bucketCount, sessionCount int
	_ = s.Store.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM buckets`).Scan(&bucketCount)
	_ = s.Store.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&sessionCount)
	res.Aggregate = routes.DebugAggregateInfo{
		BucketCount:  bucketCount,
		SessionCount: sessionCount,
	}
	if b, err := s.Store.LatestBucket(ctx, acct); err == nil && b != nil {
		res.Aggregate.LatestBucket = &routes.DebugBucketSummary{
			StartTS:       time.UnixMilli(b.StartUnixMS).UTC().Format(time.RFC3339),
			EndTS:         time.UnixMilli(b.EndUnixMS).UTC().Format(time.RFC3339),
			ResetInferred: b.ResetInferred,
			RawTokens:     b.RawTokenTotal,
			CostWeighted:  round2(b.CostWeightedTotal),
			OutputTokens:  b.OutputTokenTotal,
			TurnCount:     b.TurnCount,
		}
	}

	writeJSON(w, http.StatusOK, res)
}
