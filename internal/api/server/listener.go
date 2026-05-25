package server

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
)

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
