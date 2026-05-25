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
	"os"
	"path/filepath"
)

const socketFile = "api.sock"

// SocketPath returns the unix-socket path the daemon listens on.
//
// We use $XDG_RUNTIME_DIR/bloodhound/api.sock. systemd-logind sets that
// var for every logged-in user on Linux, so in practice it's always
// present. If it isn't, we error rather than guessing a /tmp fallback —
// cross-platform path resolution (macOS, Windows) lands when those ports
// land.
func SocketPath() (string, error) {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		return "", errors.New("XDG_RUNTIME_DIR is not set; cannot pick a socket path")
	}
	return filepath.Join(dir, "bloodhound", socketFile), nil
}
