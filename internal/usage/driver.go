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

	"github.com/PeterSR/claude-code-bloodhound/internal/pty"
)

// promptChar is the cursor character claude renders at the start of any
// interactive row — both the main input box and modal menu options.
// Distinguishing the two from a raw byte stream is unreliable; the
// VT-grid path in hasInputPrompt does it row-shape-aware.
const promptChar = "❯"

// HasInputPrompt reports whether the rendered grid contains a row that
// looks like claude's main input prompt: a "❯" followed by either
// nothing else or just a placeholder suggestion (Try "..."). Menu rows
// like "❯ 1. Yes, I trust this folder" don't match.
//
// Exported so the selfheal package can reuse the same detector for the
// outer orchestrator pty.
func HasInputPrompt(screen string) bool {
	for _, line := range strings.Split(screen, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == promptChar {
			return true
		}
		if strings.HasPrefix(trimmed, promptChar+" Try ") {
			return true
		}
	}
	return false
}

// panelMarker is the one string every rendered /usage panel carries, on
// every gauge row. drive already waits on it to decide the panel finished
// rendering (see the sentExit branch), so reusing it here keeps "did the
// panel render" one definition rather than two that can disagree.
const panelMarker = "% used"

// PanelCaptured reports whether a captured screen actually contains the
// /usage panel, as opposed to the panel's loading state or whatever else
// claude happened to be showing.
//
// This is the difference between the two ways extraction can come up
// empty, which look identical from the missing-fields list alone:
//
//   - The panel rendered but the extractor's regexes no longer match it.
//     That is extractor drift, and a self-heal is exactly the right
//     response.
//   - The panel never rendered at all, because the data fetch behind it
//     was still in flight when drive hit its deadline and captured a
//     screen reading "Refreshing…". No extractor can pull percentages off
//     a screen that has none, so retraining against it is guaranteed
//     waste: it spends ~30s driving a second claude in a pty, on the
//     user's own subscription, to rewrite regexes that were never wrong.
//
// Callers gate self-heal on this so only the first case triggers one.
func PanelCaptured(screen string) bool {
	return strings.Contains(screen, panelMarker)
}

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

	// panelBytes is the capture as it stood just before we sent Ctrl-C on a
	// rendered panel. That, not the full stream, is what we hand back:
	// since Claude Code 2.1.280 dismissing the settings dialog redraws the
	// screen, and on a panel taller than the terminal the redraw erases the
	// gauge rows while leaving the rest of the panel behind. The full
	// stream then renders without a single "% used", every poll.
	var panelBytes []byte
	result := func() []byte {
		b := cleanup()
		if panelBytes != nil {
			return panelBytes
		}
		return b
	}

	for {
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return result(), ctx.Err()
		case <-readDone:
			return result(), nil
		case <-time.After(200 * time.Millisecond):
		}

		curBytes, sinceLast := snapshot()

		if !trustHandled {
			// Render the current capture through the VT grid so visual
			// spacing is preserved and overdrawn text is dropped. Match
			// against natural human-readable markers.
			screen := strings.ToLower(renderVTVisible(curBytes))
			hasTrustModal := strings.Contains(screen, "trust this folder")
			welcomeReady := strings.Contains(screen, "tips for getting") ||
				strings.Contains(screen, "claude code v") ||
				strings.Contains(screen, "what's new")

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
			// Use the VT grid to detect the input prompt structurally:
			// claude's main input row ends in "❯ <cursor>" and otherwise
			// has nothing past it. Menu cursors (the ❯ in trust prompts
			// etc.) are followed by their option text, so don't match.
			screen := renderVTVisible(curBytes)
			ready := HasInputPrompt(screen)
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
			// Render the VT grid before scanning: raw bytes have escape
			// sequences between any two characters, and the grid drops
			// stale overdrawn text that could falsely match. The /usage
			// panel always shows "% used" with a real space.
			screen := renderVTVisible(curBytes)
			panelRendered := PanelCaptured(screen)
			if time.Since(typedAt) > minRenderAfterType && panelRendered {
				time.Sleep(700 * time.Millisecond)
				panelBytes, _ = snapshot()
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

	return result(), nil
}
