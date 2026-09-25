// Package account works out which Claude account a config dir is logged in
// to, from Claude Code's own state file, and records it in the store's login
// history so turns and /usage readings can be attributed per account.
//
// Transcripts carry no account id, so this is the only source. It is read on
// every ingest and every poll, and a switch counts from the moment it is first
// seen. That is imprecise around a /login and accepted as such.
package account

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/config"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

// Identity is a dir's current login, as the state file reports it.
type Identity struct {
	store.AccountIdentity
	// OAuth is false for a dir with no subscription login (API key, Bedrock,
	// Vertex). Such a dir gets a synthetic per-dir account and has no /usage
	// meter to poll.
	OAuth bool
}

// StateFile is where Claude Code keeps a config dir's global state. With
// CLAUDE_CONFIG_DIR unset that is ~/.claude.json, beside the dir rather than
// in it; with it set, it is inside the dir.
func StateFile(dir string) string {
	if config.IsDefaultClaudeDir(dir) {
		return filepath.Join(filepath.Dir(dir), ".claude.json")
	}
	return filepath.Join(dir, ".claude.json")
}

type stateFile struct {
	OAuthAccount *struct {
		AccountUUID               string `json:"accountUuid"`
		EmailAddress              string `json:"emailAddress"`
		OrganizationUUID          string `json:"organizationUuid"`
		OrganizationName          string `json:"organizationName"`
		OrganizationRateLimitTier string `json:"organizationRateLimitTier"`
		UserRateLimitTier         string `json:"userRateLimitTier"`
	} `json:"oauthAccount"`
}

// Read reports who dir is logged in to. A missing state file, or one with no
// OAuth login, is not an error: it yields the dir's synthetic account. A state
// file that exists but will not parse is an error, because Claude Code
// rewrites it constantly and a torn read must not be mistaken for a logout.
func Read(dir string) (Identity, error) {
	synthetic := Identity{AccountIdentity: store.AccountIdentity{AccountUUID: "dir:" + dir}}

	data, err := os.ReadFile(StateFile(dir))
	if os.IsNotExist(err) {
		return synthetic, nil
	}
	if err != nil {
		return Identity{}, err
	}
	var sf stateFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return Identity{}, fmt.Errorf("parse %s: %w", StateFile(dir), err)
	}
	oa := sf.OAuthAccount
	if oa == nil || oa.AccountUUID == "" {
		return synthetic, nil
	}
	tier := oa.OrganizationRateLimitTier
	if tier == "" {
		tier = oa.UserRateLimitTier
	}
	return Identity{
		AccountIdentity: store.AccountIdentity{
			AccountUUID:   oa.AccountUUID,
			OrgUUID:       oa.OrganizationUUID,
			Email:         oa.EmailAddress,
			OrgName:       oa.OrganizationName,
			RateLimitTier: tier,
		},
		OAuth: true,
	}, nil
}

// Observe reads dir's login and records it, returning the account id.
func Observe(ctx context.Context, s *store.Store, dir string, now time.Time) (int64, Identity, error) {
	ident, err := Read(dir)
	if err != nil {
		return 0, Identity{}, err
	}
	id, err := s.ObserveLogin(ctx, dir, config.IsDefaultClaudeDir(dir), ident.AccountIdentity, now.UnixMilli())
	if err != nil {
		return 0, Identity{}, err
	}
	return id, ident, nil
}
