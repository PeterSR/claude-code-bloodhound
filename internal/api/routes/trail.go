package routes

// TrailResponse is the payload for the Trail page: per-session briefs,
// the by-repo pivot, and the analyzer's own cost.
type TrailResponse struct {
	Enabled     bool             `json:"enabled"`
	Mode        string           `json:"mode"`
	IntervalS   int              `json:"interval_s"`
	ServerNowMS int64            `json:"server_now_ms"`
	Sessions    []TrailSession   `json:"sessions"`
	Repos       []TrailRepoGroup `json:"repos"`
	Cost        TrailCost        `json:"cost"`
}

// TrailSession is one session's current brief plus its repos + loops.
type TrailSession struct {
	SessionUUID   string         `json:"session_uuid"`
	Project       string         `json:"project"`
	Cwd           string         `json:"cwd"`
	Headline      string         `json:"headline"`
	Summary       string         `json:"summary"`
	UpdatedUnixMS int64          `json:"updated_unix_ms"`
	AnalyzedRuns  int            `json:"analyzed_runs"`
	Repos         []TrailRepoRef `json:"repos"`
	OpenLoops     []TrailLoop    `json:"open_loops"`
}

// TrailRepoRef is one worktree a session touched, with live-derived
// display fields.
type TrailRepoRef struct {
	Path    string `json:"path"`
	Dirname string `json:"dirname"`
	Branch  string `json:"branch"`
	Role    string `json:"role"`
}

// TrailLoop is one outstanding item. Status is the effective status
// (user override wins); AnalyzerStatus is the model's last view.
type TrailLoop struct {
	SessionUUID     string `json:"session_uuid"`
	Key             string `json:"key"`
	Text            string `json:"text"`
	Status          string `json:"status"`
	AnalyzerStatus  string `json:"analyzer_status"`
	UserStatus      string `json:"user_status,omitempty"`
	UserNote        string `json:"user_note,omitempty"`
	RelatedRepoPath string `json:"related_repo_path,omitempty"`
	FirstSeenUnixMS int64  `json:"first_seen_unix_ms"`
	LastSeenUnixMS  int64  `json:"last_seen_unix_ms"`
}

// TrailRepoGroup is the by-repo pivot: one worktree and the sessions
// touching it (the cross-cutting view).
type TrailRepoGroup struct {
	Path      string                `json:"path"`
	Dirname   string                `json:"dirname"`
	Branch    string                `json:"branch"`
	CommonDir string                `json:"common_dir,omitempty"`
	Sessions  []TrailRepoSessionRef `json:"sessions"`
}

// TrailRepoSessionRef links a repo group back to a session.
type TrailRepoSessionRef struct {
	SessionUUID string `json:"session_uuid"`
	Headline    string `json:"headline"`
	Role        string `json:"role"`
}

// TrailCost is the analyzer's aggregate resource use, expressed both in
// tokens and (when calibration is available) as a share of the week.
type TrailCost struct {
	Runs               int64   `json:"runs"`
	CostUSD            float64 `json:"cost_usd"`
	InputTokens        int64   `json:"input_tokens"`
	OutputTokens       int64   `json:"output_tokens"`
	CacheReadTokens    int64   `json:"cache_read_tokens"`
	CacheCreateTokens  int64   `json:"cache_create_tokens"`
	CostWeightedTokens float64 `json:"cost_weighted_tokens"`
	PctOfWeek          float64 `json:"pct_of_week,omitempty"`
}

// TrailResolveRequest crosses off (or un-crosses) a loop manually.
// UserStatus "" clears the override; "done"/"dismissed" set it.
type TrailResolveRequest struct {
	SessionUUID string `json:"session_uuid"`
	LoopKey     string `json:"loop_key"`
	UserStatus  string `json:"user_status"`
	Note        string `json:"note"`
}
