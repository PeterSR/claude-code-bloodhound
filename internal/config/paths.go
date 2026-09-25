// Package config resolves per-OS paths and loads/saves the JSON config file.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const appName = "bloodhound"

// DataDir returns the directory where bloodhound stores its database and other
// persistent data. Created lazily by callers that need it.
//
// Linux:   $XDG_DATA_HOME/bloodhound  (defaults to $HOME/.local/share/bloodhound)
// macOS:   $HOME/Library/Application Support/bloodhound
// Windows: %LOCALAPPDATA%\bloodhound
func DataDir() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", appName), nil
	case "windows":
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			base = filepath.Join(home, "AppData", "Local")
		}
		return filepath.Join(base, appName), nil
	default:
		// Linux + BSDs: XDG-style.
		base := os.Getenv("XDG_DATA_HOME")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			base = filepath.Join(home, ".local", "share")
		}
		return filepath.Join(base, appName), nil
	}
}

// ConfigDir returns the directory where bloodhound stores its config file.
// Uses os.UserConfigDir, which already implements XDG / Apple / Windows
// conventions.
func ConfigDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, appName), nil
}

// CacheDir returns the directory where bloodhound stores transient files
// (e.g. logs, scratch parses). Currently unused but reserved.
func CacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, appName), nil
}

// StateDir returns the directory where bloodhound stores derived state
// (e.g. learned /usage extractors and snapshots). XDG defines a "state"
// directory distinct from data and config; on platforms that don't have
// the concept we fall back to DataDir.
//
// Linux:   $XDG_STATE_HOME/bloodhound  (defaults to $HOME/.local/state/bloodhound)
// macOS:   $HOME/Library/Application Support/bloodhound  (no separate state convention)
// Windows: %LOCALAPPDATA%\bloodhound                      (no separate state convention)
func StateDir() (string, error) {
	switch runtime.GOOS {
	case "darwin", "windows":
		return DataDir()
	default:
		base := os.Getenv("XDG_STATE_HOME")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			base = filepath.Join(home, ".local", "state")
		}
		return filepath.Join(base, appName), nil
	}
}

// ClaudeProjectsDir returns where the default config dir keeps its session
// JSONL files. Only for callers that are not account-aware yet; ingest walks
// <dir>/projects for every dir in ClaudeConfigDirs.
func ClaudeProjectsDir() (string, error) {
	d, err := DefaultClaudeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "projects"), nil
}

// DefaultClaudeDir is Claude Code's config dir when CLAUDE_CONFIG_DIR is
// unset: ~/.claude.
func DefaultClaudeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}

// ClaudeConfigDirs resolves cfg.ClaudeDirs to absolute, cleaned, de-duplicated
// paths, in the order given. Empty means the default dir alone.
func ClaudeConfigDirs(cfg Config) ([]string, error) {
	if len(cfg.ClaudeDirs) == 0 {
		d, err := DefaultClaudeDir()
		if err != nil {
			return nil, err
		}
		return []string{d}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, d := range cfg.ClaudeDirs {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		if d == "~" {
			d = home
		} else if strings.HasPrefix(d, "~/") {
			d = filepath.Join(home, d[2:])
		}
		d, err = filepath.Abs(d)
		if err != nil {
			return nil, err
		}
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out, nil
}

// IsDefaultClaudeDir reports whether dir is ~/.claude, the dir Claude Code
// uses with CLAUDE_CONFIG_DIR unset. That dir keeps its state file one level
// up, at ~/.claude.json, and needs no env var to address.
func IsDefaultClaudeDir(dir string) bool {
	d, err := DefaultClaudeDir()
	return err == nil && filepath.Clean(dir) == d
}

// EnsureDir creates dir (and any parents) with sensible permissions.
func EnsureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	return nil
}

// DaemonLogPath returns the default file path for daemon stdout/stderr,
// living under the state directory because logs are persistent state but
// not load-bearing. Override via the daemon's --log-file flag.
//
// Linux:   $XDG_STATE_HOME/bloodhound/daemon.log
// macOS:   $HOME/Library/Application Support/bloodhound/daemon.log
// Windows: %LOCALAPPDATA%\bloodhound\daemon.log
func DaemonLogPath() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.log"), nil
}
