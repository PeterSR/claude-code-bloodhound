package account

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateFile_DefaultDirLivesBesideIt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got, want := StateFile(filepath.Join(home, ".claude")), filepath.Join(home, ".claude.json"); got != want {
		t.Errorf("default dir state file = %s, want %s", got, want)
	}
	work := filepath.Join(home, ".claude-work")
	if got, want := StateFile(work), filepath.Join(work, ".claude.json"); got != want {
		t.Errorf("custom dir state file = %s, want %s", got, want)
	}
}

func TestRead(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", t.TempDir())

	// No state file: synthetic per-dir account, no meter.
	id, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if id.OAuth || id.AccountUUID != "dir:"+dir {
		t.Errorf("missing state file = %+v, want synthetic dir account", id)
	}

	write := func(s string) {
		if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write(`{"oauthAccount":{"accountUuid":"acc-1","organizationUuid":"org-1","emailAddress":"a@example.com","organizationName":"Example","userRateLimitTier":"default_claude_max_5x"}}`)
	id, err = Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !id.OAuth || id.AccountUUID != "acc-1" || id.OrgUUID != "org-1" || id.RateLimitTier != "default_claude_max_5x" {
		t.Errorf("logged-in state file = %+v", id)
	}

	// Logged out (or API key): synthetic again.
	write(`{"primaryApiKey":"x"}`)
	if id, err = Read(dir); err != nil || id.OAuth {
		t.Errorf("no oauthAccount = %+v, %v; want synthetic, no error", id, err)
	}

	// Torn write: an error, never a logout.
	write(`{"oauthAccount":{"accountUu`)
	if _, err := Read(dir); err == nil {
		t.Error("torn state file read as a valid login")
	}
}
