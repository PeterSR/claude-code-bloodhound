// Package config resolves per-OS paths and loads/saves the JSON config file.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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

// ClaudeProjectsDir returns the path where Claude Code stores its session
// JSONL files. We assume Anthropic's CLI uses ~/.claude/projects on every
// platform; if that turns out to be wrong elsewhere this is the place to
// fix it.
func ClaudeProjectsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

// EnsureDir creates dir (and any parents) with sensible permissions.
func EnsureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	return nil
}
