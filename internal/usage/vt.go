package usage

import (
	"io"
	"strings"

	"github.com/charmbracelet/x/vt"
)

// vtCols / vtRows define the virtual terminal we replay the captured
// pty bytes into. Match the size we ask the real pty for in driver_unix
// so claude's layout decisions stay consistent between what the user
// would see and what we feed the extractor.
const (
	vtCols = 200
	vtRows = 60
)

// renderVT replays the raw pty byte stream into a virtual terminal grid
// and returns the resulting text, row by row, with trailing whitespace
// trimmed and visual gaps preserved as spaces.
//
// This replaces the old `stripANSI`-then-regex flow for extraction.
// Two reasons it matters:
//
//   - claude's TUI positions characters via ANSI cursor-move codes
//     instead of writing literal spaces. After plain stripANSI the words
//     "Current session" arrive as "Currentsession", forcing every
//     extractor regex to work around the missing whitespace. The grid
//     re-introduces real spaces between visually-separated characters.
//
//   - claude repeatedly redraws the screen — every status-line tick,
//     every settle, every welcome panel update. stripANSI accumulates
//     every write ever performed, including stale "loading…" text that
//     can produce false regex matches. The grid only keeps the latest
//     content at each cell, so the extractor sees what was actually on
//     screen at the end of the capture.
func renderVT(raw []byte) string {
	e := vt.NewEmulator(vtCols, vtRows)
	// The emulator writes ANSI responses (e.g. for InBandResize when
	// claude enables it) to an internal io.Pipe. Without a reader,
	// those writes block Write() forever — and we'd hang the entire
	// drive loop. Drain into Discard, then close to release the
	// goroutine once we've consumed all input.
	drained := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, e)
		close(drained)
	}()
	_, _ = e.Write(raw)
	_ = e.Close()
	<-drained

	var b strings.Builder
	for y := 0; y < vtRows; y++ {
		var row strings.Builder
		for x := 0; x < vtCols; x++ {
			cell := e.CellAt(x, y)
			if cell == nil {
				row.WriteByte(' ')
				continue
			}
			s := cell.String()
			if s == "" {
				row.WriteByte(' ')
				continue
			}
			row.WriteString(s)
		}
		b.WriteString(strings.TrimRight(row.String(), " "))
		b.WriteByte('\n')
	}
	return b.String()
}
