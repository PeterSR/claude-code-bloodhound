package routes

// LeaksResponse aggregates avoidable spend across all sessions.
//
// "Avoidable" here is shorthand for: the user could plausibly change a
// behavior or workflow setting and pay less. We don't claim these are
// 100% wasted — sometimes you really do need to rotate breakpoints — but
// they're the ones worth surfacing first.
type LeaksResponse struct {
	IdleMissTokens       int64 `json:"idle_miss_tokens"`
	IdleMissTurnCount    int   `json:"idle_miss_turn_count"`
	RotationTokens       int64 `json:"rotation_tokens"`
	RotationTurnCount    int   `json:"rotation_turn_count"`
	RestructureTokens    int64 `json:"restructure_tokens"`
	RestructureCount     int   `json:"restructure_turn_count"`
	ColdCompactionTokens int64 `json:"cold_compaction_tokens"`
	ColdCompactionCount  int   `json:"cold_compaction_count"`

	TotalRawTokens int64   `json:"total_raw_tokens"`
	TotalCWTokens  float64 `json:"total_cw_tokens"`

	TopOffenders []SessionLeakRow `json:"top_offenders"`
}

// SessionLeakRow is the per-session ranking on the Leaks page.
type SessionLeakRow struct {
	SessionUUID         string `json:"session_uuid"`
	Project             string `json:"project"`
	IdleMissCount       int    `json:"idle_miss_count"`
	RotationCount       int    `json:"rotation_count"`
	RestructureCount    int    `json:"restructure_count"`
	ColdCompactionCount int    `json:"cold_compaction_count"`
	AvoidableScore      int64  `json:"avoidable_score"` // sum of leak tokens for this session
	RawTokens           int64  `json:"raw_tokens"`
}
