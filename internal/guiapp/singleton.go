//go:build unix

// Package guiapp implements the bits of bloodhound-gui that don't depend
// on the webview itself: a single-instance lock, an IPC channel for
// "focus the existing window" messages, and (in cmd/bloodhound-gui's own
// gtk glue) the cgo plumbing to actually raise that window.
//
// The lock is a flock on $XDG_RUNTIME_DIR/bloodhound-gui.lock; the IPC is
// a unix datagram socket at $XDG_RUNTIME_DIR/bloodhound-gui.sock. Both
// disappear on process death (flock is process-scoped; the socket is
// re-bound by the next primary that wins the lock), so a crash never
// leaves a stuck lockfile.
package guiapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// FocusMessage is what a second-instance launch writes to the primary's
// socket. ActivationToken comes from XDG_ACTIVATION_TOKEN — Gnome/Wayland
// passes it via env when the user re-clicks the dock icon, and threading
// it through is what makes the raise actually focus instead of just
// nudging the taskbar.
type FocusMessage struct {
	ActivationToken string `json:"activation_token,omitempty"`
}

const (
	lockBase   = "bloodhound-gui.lock"
	socketBase = "bloodhound-gui.sock"
)

func runtimeDir() (string, error) {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		// Fall back to /tmp; not as good (world-writable, no per-user
		// scoping) but lets the GUI still launch in minimal envs.
		dir = filepath.Join(os.TempDir(), fmt.Sprintf("bloodhound-%d", os.Getuid()))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// state is the package-level handle to the lock + listener owned by the
// primary instance. Held for the lifetime of the process.
var state struct {
	mu       sync.Mutex
	lockFile *os.File
	listener *net.UnixListener
}

// Bootstrap acquires the singleton lock. It returns isPrimary=true when
// this process is the first instance (caller should proceed to open a
// window); isPrimary=false means another instance is running and the
// caller should call SendFocusToPrimary then exit.
//
// The returned cleanup must be deferred whether primary or not — for
// the primary it releases the lock and removes the socket; for the
// secondary it's a no-op.
func Bootstrap() (isPrimary bool, cleanup func(), err error) {
	dir, err := runtimeDir()
	if err != nil {
		return false, func() {}, err
	}
	lockPath := filepath.Join(dir, lockBase)

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, func() {}, fmt.Errorf("open lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			// Another instance holds the lock — we're the secondary.
			return false, func() {}, nil
		}
		return false, func() {}, fmt.Errorf("flock: %w", err)
	}

	state.mu.Lock()
	state.lockFile = f
	state.mu.Unlock()

	cleanup = func() {
		state.mu.Lock()
		defer state.mu.Unlock()
		if state.listener != nil {
			_ = state.listener.Close()
			state.listener = nil
		}
		if state.lockFile != nil {
			_ = syscall.Flock(int(state.lockFile.Fd()), syscall.LOCK_UN)
			_ = state.lockFile.Close()
			state.lockFile = nil
		}
	}
	return true, cleanup, nil
}

// SendFocusToPrimary opens the singleton socket and writes a focus
// message including XDG_ACTIVATION_TOKEN if present. Best-effort:
// returns nil even on partial failure, since "couldn't focus the
// window" is a soft error that shouldn't break dock launches.
func SendFocusToPrimary(ctx context.Context) error {
	dir, err := runtimeDir()
	if err != nil {
		return err
	}
	sockPath := filepath.Join(dir, socketBase)

	// Brief retry: the primary may still be binding when we arrive.
	var conn net.Conn
	deadline := time.Now().Add(2 * time.Second)
	for {
		var dialErr error
		conn, dialErr = net.DialTimeout("unix", sockPath, 200*time.Millisecond)
		if dialErr == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("dial primary: %w", dialErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	defer conn.Close()

	msg := FocusMessage{ActivationToken: os.Getenv("XDG_ACTIVATION_TOKEN")}
	enc := json.NewEncoder(conn)
	if err := enc.Encode(msg); err != nil {
		return fmt.Errorf("send focus: %w", err)
	}
	return nil
}

// ListenForFocus binds the singleton socket and dispatches incoming
// FocusMessage values to onFocus on a background goroutine. The caller
// should marshal back to the GUI thread inside onFocus (e.g. via
// webview's Dispatch).
//
// Only valid to call after Bootstrap returned isPrimary=true.
func ListenForFocus(onFocus func(FocusMessage)) error {
	dir, err := runtimeDir()
	if err != nil {
		return err
	}
	sockPath := filepath.Join(dir, socketBase)

	// Remove a stale socket file from a prior crashed primary; flock
	// already proved we're the only live owner.
	_ = os.Remove(sockPath)

	addr, err := net.ResolveUnixAddr("unix", sockPath)
	if err != nil {
		return fmt.Errorf("resolve sock: %w", err)
	}
	l, err := net.ListenUnix("unix", addr)
	if err != nil {
		return fmt.Errorf("bind sock: %w", err)
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		_ = l.Close()
		return fmt.Errorf("chmod sock: %w", err)
	}

	state.mu.Lock()
	state.listener = l
	state.mu.Unlock()

	go acceptFocusLoop(l, onFocus)
	return nil
}

func acceptFocusLoop(l *net.UnixListener, onFocus func(FocusMessage)) {
	for {
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// Transient error — back off briefly.
			time.Sleep(50 * time.Millisecond)
			continue
		}
		go handleFocusConn(conn, onFocus)
	}
}

func handleFocusConn(conn net.Conn, onFocus func(FocusMessage)) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	dec := json.NewDecoder(conn)
	var msg FocusMessage
	if err := dec.Decode(&msg); err != nil && !errors.Is(err, io.EOF) {
		return
	}
	onFocus(msg)
}
