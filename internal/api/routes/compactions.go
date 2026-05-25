package routes

// CompactionItem is the public JSON shape per row.
type CompactionItem struct {
	ID               int64    `json:"id"`
	SessionUUID      string   `json:"session_uuid"`
	Project          string   `json:"project"`
	TS               string   `json:"ts"`
	TSUnixMS         int64    `json:"ts_unix_ms"`
	PrefixTokensEst  int      `json:"prefix_tokens_est"`
	SummaryTokensEst int      `json:"summary_tokens_est"`
	GapToPrevS       *float64 `json:"gap_to_prev_s,omitempty"`
	CacheState       string   `json:"cache_state"`
	Confirmed        bool     `json:"confirmed"`
	ConfirmReason    string   `json:"confirm_reason,omitempty"`
}

// CompactionsResponse rolls up totals + lists.
type CompactionsResponse struct {
	Items        []CompactionItem `json:"items"`
	Count        int              `json:"count"`
	ByCacheState map[string]int   `json:"by_cache_state"`
	ColdCount    int              `json:"cold_count"`
	WarmCount    int              `json:"warm_count"`
	UnknownCount int              `json:"unknown_count"`
	WastedTokens int64            `json:"wasted_tokens"` // sum of prefix re-cache cost on cold compactions
}
