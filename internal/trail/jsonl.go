package trail

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Record is one session JSONL line, slimmed to what the analyzer needs:
// when it happened, who spoke, the cwd at that point, and a best-effort
// plain-text rendering of the message (tool calls collapsed to a short
// marker so the analyzer sees that work happened without the full noise).
type Record struct {
	TSUnixMS int64  `json:"ts_unix_ms"`
	Type     string `json:"type"` // user | assistant | system
	Cwd      string `json:"cwd"`
	Text     string `json:"text"`
}

// rawLine mirrors the subset of Claude Code's JSONL we parse.
type rawLine struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Cwd       string          `json:"cwd"`
	IsMeta    bool            `json:"isMeta"`
	Message   json.RawMessage `json:"message"`
}

type rawMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type contentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ReadSince streams a session JSONL and returns the records strictly
// after sinceMS, the maximum timestamp seen (the new watermark), and the
// most recent non-empty cwd. Records with no usable text are dropped.
func ReadSince(path string, sinceMS int64) (recs []Record, maxTS int64, latestCwd string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, sinceMS, "", err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 256*1024), 8*1024*1024)
	for sc.Scan() {
		var rl rawLine
		if err := json.Unmarshal(sc.Bytes(), &rl); err != nil {
			continue
		}
		if rl.IsMeta {
			continue
		}
		ts := parseTSms(rl.Timestamp)
		if ts == 0 {
			continue
		}
		if ts > maxTS {
			maxTS = ts
		}
		if rl.Cwd != "" {
			latestCwd = rl.Cwd
		}
		if ts <= sinceMS {
			continue
		}
		text := extractText(rl.Message)
		if strings.TrimSpace(text) == "" {
			continue
		}
		recs = append(recs, Record{
			TSUnixMS: ts,
			Type:     rl.Type,
			Cwd:      rl.Cwd,
			Text:     text,
		})
	}
	if maxTS < sinceMS {
		maxTS = sinceMS
	}
	return recs, maxTS, latestCwd, sc.Err()
}

// extractText renders a message's content to plain text. String content
// is returned as-is; block arrays keep text blocks and collapse tool
// calls / results to a one-line marker so the analyzer sees that a tool
// ran (and on what) without the full payload.
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m rawMsg
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	// content may be a bare string.
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	var blocks []contentBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return ""
	}
	var b strings.Builder
	for _, bl := range blocks {
		switch bl.Type {
		case "text":
			b.WriteString(bl.Text)
			b.WriteByte('\n')
		case "tool_use":
			b.WriteString("[tool: " + bl.Name + " " + truncate(string(bl.Input), 200) + "]\n")
		case "tool_result":
			// Results are usually large + low-signal for a work summary;
			// just note one happened.
			b.WriteString("[tool result]\n")
		}
	}
	return b.String()
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func parseTSms(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		// Some lines carry fractional seconds / Z; RFC3339Nano covers it.
		t, err = time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return 0
		}
	}
	return t.UnixMilli()
}

// LocateSessionJSONL finds the on-disk JSONL for a session id under the
// Claude projects dir, or "" if not found. Used to attribute the
// analyzer's own token cost in interactive mode.
func LocateSessionJSONL(projectsDir, sessionID string) string {
	matches, _ := filepath.Glob(filepath.Join(projectsDir, "*", sessionID+".jsonl"))
	if len(matches) > 0 {
		return matches[0]
	}
	return ""
}

// usageLine is the token-usage subset of an assistant record.
type usageLine struct {
	Message struct {
		Usage struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// sumUsage totals the token usage across a session's assistant turns.
// Best-effort: unreadable / missing file yields a zero Cost.
func sumUsage(path string) Cost {
	f, err := os.Open(path)
	if err != nil {
		return Cost{}
	}
	defer f.Close()
	var c Cost
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 256*1024), 8*1024*1024)
	for sc.Scan() {
		var u usageLine
		if err := json.Unmarshal(sc.Bytes(), &u); err != nil {
			continue
		}
		c.InputTokens += u.Message.Usage.InputTokens
		c.OutputTokens += u.Message.Usage.OutputTokens
		c.CacheReadTokens += u.Message.Usage.CacheReadInputTokens
		c.CacheCreateTokens += u.Message.Usage.CacheCreationInputTokens
	}
	return c
}
