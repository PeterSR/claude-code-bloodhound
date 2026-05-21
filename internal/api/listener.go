package api

import (
	"errors"
	"fmt"
	"net"
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

// Listen prepares the parent directory, removes any stale socket file
// from a previous crash, binds an AF_UNIX listener, and tightens
// permissions to 0600 so only the owning user can talk to the API.
func Listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	// A previous crash can leave the socket file on disk; net.Listen
	// would refuse to bind on top of it. If another daemon is actually
	// live we'd still race here — a flock-based lock is the proper fix,
	// but is out of scope for the "one user, one daemon" world today.
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}
