package routes

// DebugResponse is the kitchen-sink "is this thing on" payload.
type DebugResponse struct {
	Build     DebugBuildInfo     `json:"build"`
	Paths     DebugPathsInfo     `json:"paths"`
	DeviceID  string             `json:"device_id"`
	Schema    int                `json:"schema_version"`
	Extractor DebugExtractorInfo `json:"extractor"`
	Ingest    DebugIngestInfo    `json:"ingest"`
	LastPoll  *DebugLastPollInfo `json:"last_poll,omitempty"`
	RawDump   *DebugRawDumpInfo  `json:"raw_dump,omitempty"`
	Aggregate DebugAggregateInfo `json:"aggregate"`
	ServerNow string             `json:"server_now"`
}

type DebugBuildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

type DebugPathsInfo struct {
	DataDir        string `json:"data_dir"`
	StateDir       string `json:"state_dir"`
	ConfigDir      string `json:"config_dir"`
	ConfigFile     string `json:"config_file"`
	ClaudeProjects string `json:"claude_projects"`
	ExtractorFile  string `json:"extractor_file"`
	ExtractorSnap  string `json:"extractor_snapshot"`
	DBPath         string `json:"db_path"`
}

type DebugExtractorInfo struct {
	Origin      string `json:"origin"`
	Version     int    `json:"version"`
	GeneratedAt string `json:"generated_at,omitempty"`
	GeneratedBy string `json:"generated_by,omitempty"`
	FieldCount  int    `json:"field_count"`
}

type DebugIngestInfo struct {
	FilesTracked     int    `json:"files_tracked"`
	TurnsTotal       int    `json:"turns_total"`
	CompactionsTotal int    `json:"compactions_confirmed"`
	LatestTurnTS     string `json:"latest_turn_ts,omitempty"`
}

type DebugLastPollInfo struct {
	TS              string  `json:"ts"`
	AgeS            int64   `json:"age_s"`
	ParseOK         bool    `json:"parse_ok"`
	ElapsedS        float64 `json:"elapsed_s"`
	SessionPct      *int    `json:"session_pct,omitempty"`
	WeekPct         *int    `json:"week_pct,omitempty"`
	SessionResetRaw string  `json:"session_reset_raw,omitempty"`
	WeekResetRaw    string  `json:"week_reset_raw,omitempty"`
}

type DebugRawDumpInfo struct {
	TS    string `json:"ts"`
	Tail  string `json:"tail"`        // last ~3000 chars of cleaned terminal text
	Bytes int    `json:"bytes_total"` // total length before truncation
}

type DebugAggregateInfo struct {
	BucketCount  int                 `json:"bucket_count"`
	SessionCount int                 `json:"session_count"`
	LatestBucket *DebugBucketSummary `json:"latest_bucket,omitempty"`
}

type DebugBucketSummary struct {
	StartTS       string  `json:"start_ts"`
	EndTS         string  `json:"end_ts"`
	ResetInferred bool    `json:"reset_inferred"`
	RawTokens     int64   `json:"raw_tokens"`
	CostWeighted  float64 `json:"cost_weighted_tokens"`
	OutputTokens  int64   `json:"output_tokens"`
	TurnCount     int     `json:"turn_count"`
}
