//go:build linux || darwin || freebsd || openbsd || netbsd

package usage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"sync"
	"time"

	"github.com/creack/pty"
)

// promptByte is the prompt character the Claude Code TUI prints when the
// input box is ready to accept keystrokes.
const promptByte = "\xe2\x9d\xaf" // ❯

// drive spawns claude in a pty, types /usage, lets the panel render, and
// returns the captured raw bytes. Mirrors the Python POC's state machine
// (wait-for-prompt, type, wait-for-render, send Ctrl-C twice) but cleaner.
func drive(ctx context.Context, opts Options) ([]byte, error) {
	cmd := exec.CommandContext(ctx, opts.ClaudeBinary)
	ptyFile, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}

	var (
		mu        sync.Mutex
		buf       bytes.Buffer
		lastChunk = time.Now()
		readDone  = make(chan struct{})
	)

	go func() {
		defer close(readDone)
		chunk := make([]byte, 65536)
		for {
			n, err := ptyFile.Read(chunk)
			if n > 0 {
				mu.Lock()
				buf.Write(chunk[:n])
				lastChunk = time.Now()
				mu.Unlock()
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					// non-EOF read errors are fine to ignore here; we'll
					// surface them via the buffer + parse outcome
				}
				return
			}
		}
	}()

	snapshot := func() ([]byte, time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		out := append([]byte(nil), buf.Bytes()...)
		return out, time.Since(lastChunk)
	}

	cleanup := func() []byte {
		_ = ptyFile.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
		<-readDone
		out, _ := snapshot()
		return out
	}

	const (
		minRenderAfterType = 5 * time.Second
		settleAfterReady   = 400 * time.Millisecond
	)
	deadline := time.Now().Add(opts.Timeout)
	startTime := time.Now()

	typed := false
	sentExit := false
	var typedAt time.Time

	for {
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			b := cleanup()
			return b, ctx.Err()
		case <-readDone:
			return cleanup(), nil
		case <-time.After(200 * time.Millisecond):
		}

		curBytes, sinceLast := snapshot()

		if !typed {
			ready := bytes.Contains(curBytes, []byte(promptByte)) ||
				bytes.Contains(curBytes, []byte("Welcome")) ||
				bytes.Contains(curBytes, []byte("Tip"))
			if ready && sinceLast >= settleAfterReady {
				time.Sleep(500 * time.Millisecond)
				_, _ = ptyFile.Write([]byte("/usage\r"))
				typed = true
				typedAt = time.Now()
			} else if time.Since(startTime) >= 7*time.Second && len(curBytes) > 2000 {
				// Last-resort fallback: type and hope.
				_, _ = ptyFile.Write([]byte("/usage\r"))
				typed = true
				typedAt = time.Now()
			}
			continue
		}

		if !sentExit {
			if time.Since(typedAt) > minRenderAfterType && bytes.Contains(curBytes, []byte("% used")) {
				time.Sleep(700 * time.Millisecond)
				_, _ = ptyFile.Write([]byte{0x03, 0x03})
				sentExit = true
			} else if time.Until(deadline) < 3*time.Second {
				_, _ = ptyFile.Write([]byte{0x03, 0x03})
				sentExit = true
			}
			continue
		}

		if time.Until(deadline) < 1*time.Second {
			break
		}
	}

	return cleanup(), nil
}
