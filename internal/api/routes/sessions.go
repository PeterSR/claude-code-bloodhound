package routes

// SessionListItem is the public JSON shape for the Sessions list page.
type SessionListItem struct {
	SessionUUID         string `json:"session_uuid"`
	Project             string `json:"project"`
	FirstTS             string `json:"first_ts"`
	LastTS              string `json:"last_ts"`
	TurnCount           int    `json:"turn_count"`
	RawTokens           int64  `json:"raw_tokens"`
	OutputTokens        int64  `json:"output_tokens"`
	Peak5hRawTokens     int64  `json:"peak_5h_raw_tokens"`
	IdleMissCount       int    `json:"idle_miss_count"`
	RotationCount       int    `json:"rotation_count"`
	RestructureCount    int    `json:"restructure_count"`
	CompactionCount     int    `json:"compaction_count"`
	ColdCompactionCount int    `json:"cold_compaction_count"`
	CacheTTL            string `json:"cache_ttl"`
	Models              string `json:"models"`

	// Attribution is this session's share of the /usage limit meters. Zero
	// values mean the aggregator has not attributed this session yet, not
	// that it was free.
	Attribution SessionAttribution `json:"attribution"`
}

// SessionsResponse is the envelope returned by GET /api/sessions.
type SessionsResponse struct {
	Sessions []SessionListItem `json:"sessions"`
	Count    int               `json:"count"`
}
