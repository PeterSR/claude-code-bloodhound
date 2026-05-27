// Package routes is the leaf-level description of the bloodhound HTTP
// API: where the daemon listens, what path each route lives at, and
// the JSON payload types each route returns. It has no business-logic
// dependencies and is safe to import from anywhere — the GUI binary
// pulls in this package for SocketPath() without dragging in any of
// the daemon's pty / store / self-heal code.
//
// The handler implementations live in sibling package internal/api/server.
package routes

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const socketFile = "api.sock"

// SocketPath returns the unix-socket path the daemon listens on, picked
// per-OS so the daemon can start cleanly on a vanilla install of any
// supported platform:
//
//   - Linux: $XDG_RUNTIME_DIR/bloodhound/api.sock. systemd-logind sets
//     that var for every logged-in user, so it's almost always present.
//   - macOS: $HOME/Library/Caches/bloodhound/api.sock. Darwin has no
//     per-user runtime dir; Caches is the conventional spot for
//     ephemeral state. The AF_UNIX path length limit on darwin is
//     ~104 bytes including the NUL — well within reach here.
//   - Windows: %LOCALAPPDATA%\bloodhound\api.sock. AF_UNIX is
//     supported on Windows 10+ and Go's net.Listen("unix", …) routes
//     to it natively.
//   - Other Unixes (the BSDs, etc.): try XDG_RUNTIME_DIR; if unset,
//     error with the GOOS name so the failure is at least diagnosable.
func SocketPath() (string, error) {
	switch runtime.GOOS {
	case "linux":
		dir := os.Getenv("XDG_RUNTIME_DIR")
		if dir == "" {
			return "", errors.New("XDG_RUNTIME_DIR is not set; cannot pick a socket path")
		}
		return filepath.Join(dir, "bloodhound", socketFile), nil

	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("user home: %w", err)
		}
		return filepath.Join(home, "Library", "Caches", "bloodhound", socketFile), nil

	case "windows":
		dir := os.Getenv("LOCALAPPDATA")
		if dir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("user home: %w", err)
			}
			dir = filepath.Join(home, "AppData", "Local")
		}
		return filepath.Join(dir, "bloodhound", socketFile), nil

	default:
		dir := os.Getenv("XDG_RUNTIME_DIR")
		if dir == "" {
			return "", fmt.Errorf("%s: no socket-path strategy and XDG_RUNTIME_DIR is unset", runtime.GOOS)
		}
		return filepath.Join(dir, "bloodhound", socketFile), nil
	}
}
