package api

import (
	"net/http"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/config"
	"github.com/PeterSR/claude-code-bloodhound/internal/usage"
	"github.com/PeterSR/claude-code-bloodhound/internal/version"
)

// DebugResponse is the kitchen-sink "is this thing on" payload.
type DebugResponse struct {
	Build      buildInfo      `json:"build"`
	Paths      pathsInfo      `json:"paths"`
	DeviceID   string         `json:"device_id"`
	Schema     int            `json:"schema_version"`
	Extractor  extractorInfo  `json:"extractor"`
	Ingest     ingestInfo     `json:"ingest"`
	LastPoll   *lastPollInfo  `json:"last_poll,omitempty"`
	RawDump    *rawDumpInfo   `json:"raw_dump,omitempty"`
	Aggregate  aggregateInfo  `json:"aggregate"`
	ServerNow  string         `json:"server_now"`
}

type buildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

type pathsInfo struct {
	DataDir          string `json:"data_dir"`
	StateDir         string `json:"state_dir"`
	ConfigDir        string `json:"config_dir"`
	ConfigFile       string `json:"config_file"`
	ClaudeProjects   string `json:"claude_projects"`
	ExtractorFile    string `json:"extractor_file"`
	ExtractorSnap    string `json:"extractor_snapshot"`
	DBPath           string `json:"db_path"`
}

type extractorInfo struct {
	Origin      string `json:"origin"`
	Version     int    `json:"version"`
	GeneratedAt string `json:"generated_at,omitempty"`
	GeneratedBy string `json:"generated_by,omitempty"`
	FieldCount  int    `json:"field_count"`
}

type ingestInfo struct {
	FilesTracked     int    `json:"files_tracked"`
	TurnsTotal       int    `json:"turns_total"`
	CompactionsTotal int    `json:"compactions_confirmed"`
	LatestTurnTS     string `json:"latest_turn_ts,omitempty"`
}

type lastPollInfo struct {
	TS              string  `json:"ts"`
	AgeS            int64   `json:"age_s"`
	ParseOK         bool    `json:"parse_ok"`
	ElapsedS        float64 `json:"elapsed_s"`
	SessionPct      *int    `json:"session_pct,omitempty"`
	WeekPct         *int    `json:"week_pct,omitempty"`
	SessionResetRaw string  `json:"session_reset_raw,omitempty"`
	WeekResetRaw    string  `json:"week_reset_raw,omitempty"`
}

type rawDumpInfo struct {
	TS    string `json:"ts"`
	Tail  string `json:"tail"`        // last ~3000 chars of cleaned terminal text
	Bytes int    `json:"bytes_total"` // total length before truncation
}

type aggregateInfo struct {
	BucketCount   int `json:"bucket_count"`
	SessionCount  int `json:"session_count"`
	LatestBucket  *bucketSummary `json:"latest_bucket,omitempty"`
}

type bucketSummary struct {
	StartTS         string  `json:"start_ts"`
	EndTS           string  `json:"end_ts"`
	ResetInferred   bool    `json:"reset_inferred"`
	RawTokens       int64   `json:"raw_tokens"`
	CostWeighted    float64 `json:"cost_weighted_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	TurnCount       int     `json:"turn_count"`
}

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

	res := DebugResponse{
		Build: buildInfo{
			Version: version.Version,
			Commit:  version.Commit,
			Date:    version.Date,
		},
		Paths: pathsInfo{
			DataDir:        dataDir,
			StateDir:       stateDir,
			ConfigDir:      cfgDir,
			ConfigFile:     cfgFile,
			ClaudeProjects: projects,
			ExtractorFile:  extractorPath,
			ExtractorSnap:  snapPath,
			DBPath:         s.Store.Path,
		},
		DeviceID: device,
		Schema:   schema,
		ServerNow: now.UTC().Format(time.RFC3339),
	}

	if ext, origin, err := usage.LoadExtractor(); err == nil {
		res.Extractor = extractorInfo{
			Origin:      string(origin),
			Version:     ext.Version,
			GeneratedAt: ext.GeneratedAt,
			GeneratedBy: ext.GeneratedBy,
			FieldCount:  len(ext.Fields),
		}
	}

	if stats, err := s.Store.IngestStats(ctx); err == nil {
		res.Ingest = ingestInfo{
			FilesTracked:     stats.FilesTracked,
			TurnsTotal:       stats.TurnsTotal,
			CompactionsTotal: stats.CompactionsTotal,
		}
		if stats.LatestTurnTSMS > 0 {
			res.Ingest.LatestTurnTS = time.UnixMilli(stats.LatestTurnTSMS).UTC().Format(time.RFC3339)
		}
	}

	if obs, err := s.Store.LatestUsage(ctx); err == nil && obs != nil {
		res.LastPoll = &lastPollInfo{
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
		res.RawDump = &rawDumpInfo{
			TS:    time.UnixMilli(ts).UTC().Format(time.RFC3339),
			Tail:  dump,
			Bytes: len(dump),
		}
	}

	// Aggregate summary: count from sessions and buckets.
	var bucketCount, sessionCount int
	_ = s.Store.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM buckets`).Scan(&bucketCount)
	_ = s.Store.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&sessionCount)
	res.Aggregate = aggregateInfo{
		BucketCount:  bucketCount,
		SessionCount: sessionCount,
	}
	if b, err := s.Store.LatestBucket(ctx); err == nil && b != nil {
		res.Aggregate.LatestBucket = &bucketSummary{
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
