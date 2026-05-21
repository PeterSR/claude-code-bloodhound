//go:build linux || darwin || freebsd || openbsd || netbsd

package usage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/creack/pty"
)

// flattenForMatch strips whitespace and lowercases, for substring matching
// against terminal output whose characters arrive separated only by ANSI
// cursor-move codes (so post-stripANSI the words have no spaces).
func flattenForMatch(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, s)
}

// promptByte is the prompt character the Claude Code TUI prints when the
// input box is ready to accept keystrokes.
const promptByte = "\xe2\x9d\xaf" // ❯

// drive spawns claude in a pty, types /usage, lets the panel render, and
// returns the captured raw bytes. Mirrors the Python POC's state machine
// (wait-for-prompt, type, wait-for-render, send Ctrl-C twice) but cleaner.
func drive(ctx context.Context, opts Options) ([]byte, error) {
	cmd := exec.CommandContext(ctx, opts.ClaudeBinary)
	// Force a terminal-shaped env. claude checks $TERM (in addition to
	// isatty) before rendering its TUI; under systemd-user the variable
	// isn't set, so claude falls back to a non-TUI subscription notice
	// and the /usage panel is never produced. Build the child env from
	// scratch with the bits the panel actually needs, rather than
	// inheriting our parent's — keeps the spawn behaviour identical
	// whether we're launched from a shell or a service manager.
	cmd.Env = append(cmd.Environ(), "TERM=xterm-256color")
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
		graceAfterExit     = 1500 * time.Millisecond // collect trailing bytes after Ctrl-C
	)
	deadline := time.Now().Add(opts.Timeout)
	startTime := time.Now()

	// On a folder claude hasn't been trusted for, the very first thing it
	// renders is a modal: "Is this a project you trust? 1. Yes  2. No".
	// That modal happens to contain ❯ (option-1 cursor), which also
	// matches our "main prompt is ready" check below — so without this
	// gate the driver fires /usage\r into the modal, the \r confirms
	// trust, the /usage chars are swallowed, and /usage never actually
	// runs. We detect the modal text and answer Enter once before
	// letting the rest of the state machine proceed.
	trustHandled := false

	typed := false
	sentExit := false
	var typedAt, sentExitAt time.Time

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

		if !trustHandled {
			// claude's TUI positions characters via ANSI cursor moves, so
			// after stripANSI words appear without their separating
			// spaces ("trustthisfolder"). Flatten whitespace before
			// substring-matching so the marker copy stays human-readable
			// in code.
			flat := flattenForMatch(stripANSI(curBytes))
			hasTrustModal := strings.Contains(flat, "trustthisfolder")
			welcomeReady := strings.Contains(flat, "tipsforgetting") ||
				strings.Contains(flat, "claudecodev") ||
				strings.Contains(flat, "what'snew")

			if hasTrustModal && sinceLast >= settleAfterReady {
				time.Sleep(300 * time.Millisecond)
				_, _ = ptyFile.Write([]byte("\r"))
				trustHandled = true
				// Give claude a beat to dismiss the modal and start
				// rendering the welcome screen before the !typed check
				// looks for the main prompt.
				time.Sleep(1 * time.Second)
				continue
			}
			if hasTrustModal {
				// Still settling; loop again.
				continue
			}
			if welcomeReady {
				// Folder was already trusted; no modal this run.
				trustHandled = true
			} else if time.Since(startTime) > 3*time.Second {
				// No definitive signal either way — proceed and let the
				// rest of the state machine decide. If claude is still
				// in a modal we'll fail this poll and retry.
				trustHandled = true
			} else {
				continue
			}
		}

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
			// Strip ANSI before scanning: raw bytes can have escape sequences
			// between any two characters, so a literal substring check is
			// unreliable. Cleaned text consistently shows "%used" or "% used".
			cleanedSoFar := stripANSI(curBytes)
			panelRendered := strings.Contains(cleanedSoFar, "% used") ||
				strings.Contains(cleanedSoFar, "%used")
			if time.Since(typedAt) > minRenderAfterType && panelRendered {
				time.Sleep(700 * time.Millisecond)
				_, _ = ptyFile.Write([]byte{0x03, 0x03})
				sentExit = true
				sentExitAt = time.Now()
			} else if time.Until(deadline) < 3*time.Second {
				_, _ = ptyFile.Write([]byte{0x03, 0x03})
				sentExit = true
				sentExitAt = time.Now()
			}
			continue
		}

		// After Ctrl-C: short grace window for trailing output, then exit.
		// Do NOT wait for the deadline — that needlessly stretches every
		// successful poll out to the full timeout.
		if time.Since(sentExitAt) > graceAfterExit {
			break
		}
	}

	return cleanup(), nil
}
