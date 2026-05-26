//go:build linux || darwin || freebsd || openbsd || netbsd

package pty

import (
	"os"
	"os/exec"

	creack "github.com/creack/pty"
)

// unixMaster is just an *os.File. The wrapper is here so the
// cross-platform Master interface stays small and so a future Resize /
// Size call can land on both platforms behind the same surface.
type unixMaster struct {
	f *os.File
}

func (m *unixMaster) Read(p []byte) (int, error)  { return m.f.Read(p) }
func (m *unixMaster) Write(p []byte) (int, error) { return m.f.Write(p) }
func (m *unixMaster) Close() error                { return m.f.Close() }

func start(cmd *exec.Cmd) (Master, error) {
	f, err := creack.Start(cmd)
	if err != nil {
		return nil, err
	}
	return &unixMaster{f: f}, nil
}
