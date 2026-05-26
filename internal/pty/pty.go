// Package pty is bloodhound's thin abstraction over a pseudoterminal.
//
// On Unix it wraps creack/pty. On Windows it wraps charmbracelet's
// ConPTY binding. Both implementations present the same Start signature
// — `Start(cmd *exec.Cmd) (Master, error)` — so existing call sites that
// previously imported creack/pty directly can switch their import path
// and keep their cmd-lifecycle calls (Process.Kill, Wait, ProcessState)
// unchanged.
//
// What Start does, per platform:
//
//   - Unix: defers to creack/pty.Start, which opens a pty pair, sets
//     the child's stdin/stdout/stderr to the slave end, calls cmd.Start,
//     and returns the master *os.File.
//
//   - Windows: creates a ConPTY (the Windows 10+ pseudoconsole), calls
//     ConPTY.Spawn (which uses CreateProcess directly — bypassing
//     cmd.Start), then populates cmd.Process via os.FindProcess(pid) so
//     callers' cmd.Process.Kill / cmd.Process.Wait / cmd.Wait still work.
//
// The Master is an io.ReadWriteCloser: Read returns whatever the child
// wrote to its tty, Write is delivered to the child's stdin, Close
// tears down the master end and (on Unix) sends EOF to the child.
package pty

import (
	"io"
	"os/exec"
)

// Master is the parent end of a pseudoterminal.
type Master interface {
	io.Reader
	io.Writer
	io.Closer
}

// Start spawns cmd attached to a freshly-created pty and returns the
// master.
//
// Caller owns the master (Close when done) and the cmd's Process: use
// cmd.Process.Kill / cmd.Wait as before. cmd.Path, cmd.Args, cmd.Env,
// and cmd.Dir are honored on both platforms; other fields (Stdin,
// Stdout, Stderr, ExtraFiles, SysProcAttr) are ignored.
func Start(cmd *exec.Cmd) (Master, error) {
	return start(cmd)
}
