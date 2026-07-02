package usage

import (
	"io"
	"strings"

	"github.com/charmbracelet/x/vt"
)

// VTCols / VTRows define the virtual terminal we replay the captured
// pty bytes into. Match the size we ask the real pty for in driver_unix
// so claude's layout decisions stay consistent between what the user
// would see and what we feed the extractor. Exported so other packages
// (selfheal) can spawn pty's with the same dimensions and reason about
// the rendered grid shape.
const (
	VTCols = 200
	VTRows = 60
)

// Two render variants exist because the byte stream is read for two
// different jobs:
//
//   - RenderVT (scrollback + visible grid) is the extraction payload:
//     what the extractor regexes run against, and what the self-heal
//     orchestrator inspects. It must include rows that scrolled above the
//     viewport (see below).
//   - RenderVTVisible (visible grid only) is for the drive-loop readiness
//     detectors (trust modal, input-prompt, panel-rendered). Those are
//     tuned to answer "what is on screen right now"; feeding them
//     scrolled-off history could match a stale transient prompt and, e.g.,
//     fire /usage early.
//
// Both replace the old `stripANSI`-then-regex flow. The grid matters for
// two reasons regardless of variant:
//
//   - claude's TUI positions characters via ANSI cursor-move codes instead
//     of writing literal spaces. After plain stripANSI the words "Current
//     session" arrive as "Currentsession", forcing every extractor regex to
//     work around the missing whitespace. The grid re-introduces real
//     spaces between visually-separated characters.
//
//   - claude repeatedly redraws the screen: every status-line tick, every
//     settle, every welcome panel update. stripANSI accumulates every write
//     ever performed, including stale "loading…" text that can produce
//     false regex matches. The grid keeps only the latest content at each
//     cell, so callers see what was actually on screen.

// renderVT / renderVTVisible are the in-package entry points; same
// behaviour as the exported RenderVT / RenderVTVisible.
func renderVT(raw []byte) string        { return RenderVT(raw) }
func renderVTVisible(raw []byte) string { return RenderVTVisible(raw) }

// RenderVT returns the scrollback buffer (rows that scrolled above the
// viewport, oldest first) followed by the current visible grid. Exported so
// selfheal (which owns its own pty session) can render against the same
// shape extraction sees.
//
// Reading scrollback matters because claude's /usage panel can render taller
// than VTRows: when it does, the "Current session … % used … Resets" gauges
// at the top scroll off the visible grid and, without this, never reach the
// extractor. The failure looks like a broken extractor but is really a
// truncated capture. Scrollback is frozen history (claude only redraws the
// live bottom of the screen), so combining it with the visible grid
// reconstructs the whole panel without reintroducing redraw noise. On the
// alternate screen the emulator keeps no scrollback (ScrollbackLen == 0), so
// this transparently degrades to visible-grid-only.
func RenderVT(raw []byte) string {
	e := newRenderedEmulator(raw)
	var b strings.Builder
	// Scrollback first: rows that scrolled above the viewport, oldest at
	// line index 0.
	writeGrid(&b, e.ScrollbackLen(), func(x, y int) string {
		c := e.ScrollbackCellAt(x, y)
		if c == nil {
			return ""
		}
		return c.String()
	})
	// Then the current visible grid.
	writeGrid(&b, VTRows, func(x, y int) string {
		c := e.CellAt(x, y)
		if c == nil {
			return ""
		}
		return c.String()
	})
	return b.String()
}

// RenderVTVisible returns only the current visible grid, without scrollback.
// Use it for readiness/prompt detection that must reason about the live
// screen, not scrolled-off history.
func RenderVTVisible(raw []byte) string {
	e := newRenderedEmulator(raw)
	var b strings.Builder
	writeGrid(&b, VTRows, func(x, y int) string {
		c := e.CellAt(x, y)
		if c == nil {
			return ""
		}
		return c.String()
	})
	return b.String()
}

// newRenderedEmulator replays raw into a fresh VT emulator and returns it
// once all input has been consumed.
func newRenderedEmulator(raw []byte) *vt.Emulator {
	e := vt.NewEmulator(VTCols, VTRows)
	// The emulator writes ANSI responses (e.g. for InBandResize when claude
	// enables it) to an internal io.Pipe. Without a reader, those writes
	// block Write() forever, and we'd hang the entire drive loop. Drain
	// into Discard, then close to release the goroutine once we've consumed
	// all input.
	drained := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, e)
		close(drained)
	}()
	_, _ = e.Write(raw)
	_ = e.Close()
	<-drained
	return e
}

// writeGrid appends rows rows, each VTCols wide, pulling each cell's rendered
// text from cellStr (which returns "" for absent/empty cells). Trailing
// whitespace is trimmed per row; interior gaps stay as spaces.
func writeGrid(b *strings.Builder, rows int, cellStr func(x, y int) string) {
	for y := 0; y < rows; y++ {
		var row strings.Builder
		for x := 0; x < VTCols; x++ {
			s := cellStr(x, y)
			if s == "" {
				row.WriteByte(' ')
				continue
			}
			row.WriteString(s)
		}
		b.WriteString(strings.TrimRight(row.String(), " "))
		b.WriteByte('\n')
	}
}
