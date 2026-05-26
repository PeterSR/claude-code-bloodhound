package selfheal

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/pty"
	"github.com/PeterSR/claude-code-bloodhound/internal/usage"
)

// Session wraps the live pty the orchestrator is driving plus the
// frozen target (which fields are required). Methods are safe to call
// from the MCP request handler goroutine while a separate reader is
// draining the pty into Session's buffer.
type Session struct {
	// ptyMaster is the host end of the pty pair. Writes go to claude's
	// stdin; reads pick up claude's terminal output.
	ptyMaster pty.Master

	// required holds the field names every saved extractor must include
	// and successfully extract against. Frozen for the session lifetime.
	required []string

	// mu guards buf, lastChunkAt.
	mu          sync.Mutex
	buf         bytes.Buffer
	lastChunkAt time.Time
	readErr     error // sticky; set when the read goroutine sees an error
	closed      bool

	// done signals to the read goroutine that it should stop.
	done chan struct{}
}

// NewSession wires a Session around an already-opened pty master and
// starts a read goroutine. The caller still owns the inner claude
// process; the Session only manipulates its tty.
func NewSession(ptyMaster pty.Master, required []string) *Session {
	s := &Session{
		ptyMaster:   ptyMaster,
		required:    required,
		lastChunkAt: time.Now(),
		done:        make(chan struct{}),
	}
	go s.readLoop()
	return s
}

// Close stops the read goroutine. Does not close ptyMaster — that's the
// caller's responsibility (matches the spawn/cleanup pattern in
// driver_unix.go where the caller owns the FD).
func (s *Session) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.done)
	s.mu.Unlock()
}

func (s *Session) readLoop() {
	chunk := make([]byte, 65536)
	for {
		select {
		case <-s.done:
			return
		default:
		}
		n, err := s.ptyMaster.Read(chunk)
		if n > 0 {
			s.mu.Lock()
			s.buf.Write(chunk[:n])
			s.lastChunkAt = time.Now()
			s.mu.Unlock()
		}
		if err != nil {
			s.mu.Lock()
			if !errors.Is(err, io.EOF) {
				s.readErr = err
			}
			s.mu.Unlock()
			return
		}
	}
}

// snapshot returns a copy of the current buffer and the duration since
// the last chunk arrived. Cheap; safe to call repeatedly.
func (s *Session) snapshot() ([]byte, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]byte(nil), s.buf.Bytes()...)
	return out, time.Since(s.lastChunkAt)
}

// renderGrid takes the current buffer and feeds it through the VT
// emulator, returning the rendered text. Walks the same code path
// extraction uses, so what claude sees == what the extractor sees.
func (s *Session) renderGrid() string {
	raw, _ := s.snapshot()
	return usage.RenderVT(raw)
}

// ReadPTY waits up to settleMs for the pty to be quiet (no new bytes
// in that window) and then snapshots. Quiet=true means we observed the
// quiet window; quiet=false means the window elapsed without one
// (claude is still actively rendering).
func (s *Session) ReadPTY(req ReadPTYRequest) ReadPTYResult {
	settle := time.Duration(req.SettleMs) * time.Millisecond
	deadline := time.Now().Add(maxReadWait)
	quiet := false
	for time.Now().Before(deadline) {
		_, since := s.snapshot()
		if since >= settle {
			quiet = true
			break
		}
		// Poll cheaply for a settle window.
		time.Sleep(50 * time.Millisecond)
	}
	grid := s.renderGrid()
	return ReadPTYResult{
		Grid:  grid,
		Cols:  usage.VTCols,
		Rows:  usage.VTRows,
		Quiet: quiet,
	}
}

// SendKeys writes text into the pty master, interpreting Go-style
// escape sequences (\r, \n, \t, \xNN, \uNNNN) on the way in. Claude's
// JSON tool args may arrive with backslash-r literally rather than a
// carriage return — match how a human would expect "\r" to behave
// when they typed it as a string literal.
func (s *Session) SendKeys(req SendKeysRequest) (SendKeysResult, error) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return SendKeysResult{}, errors.New("session closed")
	}
	text := unescapeKeys(req.Text)
	n, err := s.ptyMaster.Write([]byte(text))
	if err != nil {
		return SendKeysResult{Bytes: n}, fmt.Errorf("write pty: %w", err)
	}
	return SendKeysResult{Bytes: n}, nil
}

// unescapeKeys turns Go-style escape sequences in s into their
// corresponding bytes. Anything that doesn't parse as a Go literal
// is returned unchanged (claude probably meant the literal text).
func unescapeKeys(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	// strconv.Unquote requires surrounding quotes and handles all the
	// usual escapes (\r, \n, \t, \xNN, \uNNNN, \\, \"). Wrap and try.
	if unq, err := strconv.Unquote("\"" + escapeForUnquote(s) + "\""); err == nil {
		return unq
	}
	return s
}

// escapeForUnquote escapes characters strconv.Unquote would otherwise
// reject when we wrap s in double quotes — primarily unescaped
// double-quotes within s itself.
func escapeForUnquote(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			b.WriteString(`\"`)
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// TestRegex compiles a pattern, searches against the current rendered
// grid, and returns the capture group + a small context window.
func (s *Session) TestRegex(req TestRegexRequest) TestRegexResult {
	res := TestRegexResult{Field: req.Field}
	re, err := regexp.Compile(req.Pattern)
	if err != nil {
		res.CompileError = err.Error()
		return res
	}
	res.Compiled = true
	if req.Group < 0 || req.Group > re.NumSubexp() {
		res.CompileError = fmt.Sprintf("group %d out of range (NumSubexp=%d)", req.Group, re.NumSubexp())
		return res
	}
	grid := s.renderGrid()
	loc := re.FindStringSubmatchIndex(grid)
	if loc == nil {
		return res
	}
	res.Matched = true
	// loc has pairs of [start, end] for each group. Group 0 is whole
	// match, group N starts at 2N.
	g := req.Group
	start, end := loc[2*g], loc[2*g+1]
	if start >= 0 && end >= 0 {
		res.Value = grid[start:end]
	}
	// Context: ±60 chars around the whole match.
	wStart, wEnd := loc[0]-60, loc[1]+60
	if wStart < 0 {
		wStart = 0
	}
	if wEnd > len(grid) {
		wEnd = len(grid)
	}
	res.Context = grid[wStart:wEnd]
	return res
}

// SaveExtractor builds an usage.Extractor from the proposed rules,
// validates, dry-runs against the current grid, and persists.
func (s *Session) SaveExtractor(req SaveExtractorRequest) SaveExtractorResult {
	if len(req.Fields) == 0 {
		return SaveExtractorResult{Error: "no fields provided"}
	}
	ext := &usage.Extractor{
		Version: usage.ExtractorVersion,
		Fields:  toUsageFields(req.Fields),
	}
	if err := ext.Validate(); err != nil {
		return SaveExtractorResult{Error: "validate: " + err.Error()}
	}

	// Dry-run against the current grid. Missing required fields → reject.
	grid := s.renderGrid()
	got := ext.Apply(grid)
	if len(got.Missing) > 0 {
		return SaveExtractorResult{Missing: got.Missing}
	}

	// Also check that every field the *orchestrator was told to extract*
	// is present and successfully extracted, not just every field the
	// extractor itself marked required. Belt-and-suspenders for misaligned
	// orchestrator output.
	for _, name := range s.required {
		if _, ok := got.Values[name]; !ok {
			return SaveExtractorResult{Missing: append(got.Missing, name)}
		}
	}

	ext.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	ext.GeneratedBy = "claude"
	if err := usage.SaveExtractor(ext, grid); err != nil {
		return SaveExtractorResult{Error: "save: " + err.Error()}
	}
	savedAt, _, _ := usage.ExtractorPaths()
	return SaveExtractorResult{
		Ok:      true,
		SavedAt: savedAt,
	}
}

func toUsageFields(in []FieldRule) []usage.FieldRule {
	out := make([]usage.FieldRule, len(in))
	for i, f := range in {
		out[i] = usage.FieldRule{
			Name:     f.Name,
			Type:     f.Type,
			Regex:    f.Regex,
			Group:    f.Group,
			Required: f.Required,
		}
	}
	return out
}

// maxReadWait caps how long ReadPTY will sleep waiting for a settle
// window. Stops claude tying up tool turns with absurd settle_ms.
const maxReadWait = 10 * time.Second
