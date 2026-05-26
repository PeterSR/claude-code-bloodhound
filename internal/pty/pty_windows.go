//go:build windows

package pty

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/charmbracelet/x/conpty"
)

// Default initial console dimensions. The consumers (claude /usage
// scraper, self-heal pty driver) don't care about exact size — they
// just need a tty that produces predictable rendering. 80x25 is the
// historical fallback and what claude assumes when TERM is unset.
const (
	defaultCols = 80
	defaultRows = 25
)

type windowsMaster struct {
	cp        *conpty.ConPty
	closeOnce sync.Once
	closeErr  error
}

func (m *windowsMaster) Read(p []byte) (int, error)  { return m.cp.Read(p) }
func (m *windowsMaster) Write(p []byte) (int, error) { return m.cp.Write(p) }
func (m *windowsMaster) Close() error {
	m.closeOnce.Do(func() { m.closeErr = m.cp.Close() })
	return m.closeErr
}

func start(cmd *exec.Cmd) (Master, error) {
	if cmd.Path == "" {
		return nil, fmt.Errorf("pty: cmd.Path is empty")
	}

	cp, err := conpty.New(defaultCols, defaultRows, 0)
	if err != nil {
		return nil, fmt.Errorf("pty: conpty.New: %w", err)
	}

	attr := &syscall.ProcAttr{
		Dir: cmd.Dir,
		Env: cmd.Env,
	}

	// ConPty.Spawn calls CreateProcess directly via the pseudoconsole
	// attribute list; it does not go through cmd.Start. We then plug
	// the returned pid into a *os.Process so callers' cmd.Process.Kill,
	// cmd.Process.Wait, and cmd.Wait keep working — those just need
	// cmd.Process to be non-nil and pointing at a live handle.
	pid, _, err := cp.Spawn(cmd.Path, cmd.Args, attr)
	if err != nil {
		_ = cp.Close()
		return nil, fmt.Errorf("pty: conpty.Spawn %q: %w", cmd.Path, err)
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		_ = cp.Close()
		return nil, fmt.Errorf("pty: os.FindProcess(%d): %w", pid, err)
	}
	cmd.Process = proc

	return &windowsMaster{cp: cp}, nil
}
