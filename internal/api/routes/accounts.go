package routes

// AccountsResponse is GET /api/accounts: every Claude account bloodhound has
// seen, and which one meter-wide views show when no ?account= is given.
type AccountsResponse struct {
	OK        bool          `json:"ok"`
	PrimaryID int64         `json:"primary_id"`
	Accounts  []AccountInfo `json:"accounts"`
}

// AccountInfo is one account. Email and org come from Claude Code's own
// state file and never leave the machine.
type AccountInfo struct {
	ID            int64    `json:"id"`
	Name          string   `json:"name"` // label, else email/org, never empty
	Label         string   `json:"label,omitempty"`
	Email         string   `json:"email,omitempty"`
	OrgName       string   `json:"org_name,omitempty"`
	RateLimitTier string   `json:"rate_limit_tier,omitempty"`
	Metered       bool     `json:"metered"`               // has at least one /usage reading
	ConfigDirs    []string `json:"config_dirs,omitempty"` // dirs currently logged in to it
	LastSeenMS    int64    `json:"last_seen_ms,omitempty"`
}

// AccountLabelRequest is POST /api/accounts: set or clear a display name.
type AccountLabelRequest struct {
	ID    int64  `json:"id"`
	Label string `json:"label"`
}
