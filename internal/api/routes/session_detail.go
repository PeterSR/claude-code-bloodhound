package routes

// SessionDetailResponse is the per-session-detail payload.
type SessionDetailResponse struct {
	SessionUUID         string           `json:"session_uuid"`
	Project             string           `json:"project"`
	FirstTS             string           `json:"first_ts"`
	LastTS              string           `json:"last_ts"`
	TurnCount           int              `json:"turn_count"`
	RawTokens           int64            `json:"raw_tokens"`
	OutputTokens        int64            `json:"output_tokens"`
	Peak5hRawTokens     int64            `json:"peak_5h_raw_tokens"`
	Models              string           `json:"models"`
	CacheTTL            string           `json:"cache_ttl"`
	IdleMissCount       int              `json:"idle_miss_count"`
	RotationCount       int              `json:"rotation_count"`
	RestructureCount    int              `json:"restructure_count"`
	CompactionCount     int              `json:"compaction_count"`
	ColdCompactionCount int              `json:"cold_compaction_count"`
	Turns               []TurnItem       `json:"turns"`
	Compactions         []CompactionItem `json:"compactions"`
}

// TurnItem is the on-the-wire shape for a single turn.
type TurnItem struct {
	TurnIdx        int     `json:"turn_idx"`
	TS             string  `json:"ts"`
	TSUnixMS       int64   `json:"ts_unix_ms"`
	Model          string  `json:"model"`
	InputTokens    int     `json:"input_tokens"`
	OutputTokens   int     `json:"output_tokens"`
	CacheRead      int     `json:"cache_read"`
	CacheCreate5m  int     `json:"cache_create_5m"`
	CacheCreate1h  int     `json:"cache_create_1h"`
	GapS           float64 `json:"gap_s"`
	Classification string  `json:"classification"`
	PostCompact    bool    `json:"post_compact"`
}
