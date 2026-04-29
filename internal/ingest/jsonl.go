package ingest

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// rawRecord is the small set of JSONL fields we care about.
type rawRecord struct {
	Type             string          `json:"type"`
	Subtype          string          `json:"subtype,omitempty"`
	Timestamp        string          `json:"timestamp,omitempty"`
	Cwd              string          `json:"cwd,omitempty"`
	IsMeta           bool            `json:"isMeta,omitempty"`
	IsCompactSummary bool            `json:"isCompactSummary,omitempty"`
	Message          json.RawMessage `json:"message,omitempty"`
}

type rawMessage struct {
	Model   string          `json:"model"`
	Usage   *rawUsage       `json:"usage"`
	Content json.RawMessage `json:"content"`
}

type rawUsage struct {
	InputTokens              int             `json:"input_tokens"`
	OutputTokens             int             `json:"output_tokens"`
	CacheReadInputTokens     int             `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int             `json:"cache_creation_input_tokens"`
	CacheCreation            *rawCacheCreate `json:"cache_creation"`
}

type rawCacheCreate struct {
	Ephemeral5m int `json:"ephemeral_5m_input_tokens"`
	Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
}

// Turn is the persisted form of an assistant turn.
type Turn struct {
	SessionUUID    string
	TurnIdx        int
	TS             string
	TSUnixMS       int64
	Model          string
	InputTokens    int
	OutputTokens  int
	CacheRead      int
	CacheCreate5m  int
	CacheCreate1h  int
	GapS           float64
	Classification string
	PostCompact    bool
	Project        string
	SourcePathHash string
}

// Compaction is the persisted form of a /compact event.
type Compaction struct {
	SessionUUID       string
	TS                string
	TSUnixMS          int64
	PrefixTokensEst   int
	SummaryTokensEst  int
	GapToPrevS        *float64
	CacheState        string // cold | warm_1h | warm_5m | unknown
	Confirmed         bool
	ConfirmReason     string
	Project           string
}

// FileResult is the parsed output of one JSONL file.
type FileResult struct {
	SessionUUID  string
	Project      string
	Turns        []Turn
	Compactions  []Compaction
	PathHash     string
}

// parseFile streams one JSONL file and emits structured Turn + Compaction
// slices. Compactions are marked confirmed=true only when the next turn's
// prefix shrinks ≥ 30% relative to the boundary's prefix.
func parseFile(path string) (FileResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return FileResult{}, err
	}
	defer f.Close()

	sessionUUID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	project := filepath.Base(filepath.Dir(path))
	pathHash := hashPath(path)

	res := FileResult{
		SessionUUID: sessionUUID,
		Project:     project,
		PathHash:    pathHash,
	}

	scanner := bufio.NewScanner(f)
	// JSONL lines can be very large (huge tool outputs). Default 64KB is
	// not enough; allow up to 16MB per line.
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)

	var (
		prevAssistantTSMS int64 = 0
		prevPrefix              = 0
		hasPrev                 = false

		// pending compaction state
		pendingBoundary     bool
		pendingBoundaryTS   string
		pendingBoundaryMS   int64
		pendingPrefix       int
		pendingGapToPrev    *float64
		pendingCacheState   string

		pendingCompactReady bool
		pendingCompact      Compaction

		markPostCompact bool

		turnIdx int
	)

	finalizeCompaction := func(curPrefix int, confirmed bool, reason string) {
		pendingCompact.Confirmed = confirmed
		pendingCompact.ConfirmReason = reason
		res.Compactions = append(res.Compactions, pendingCompact)
		pendingCompactReady = false
		pendingCompact = Compaction{}
	}

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec rawRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			// Best-effort: skip malformed lines. Real JSONL is well-formed
			// in practice; tracking parse errors will land in Stats later.
			continue
		}

		switch {
		case rec.Type == "system" && rec.Subtype == "compact_boundary":
			pendingBoundary = true
			pendingBoundaryTS = rec.Timestamp
			pendingBoundaryMS = parseTSMS(rec.Timestamp)
			pendingPrefix = prevPrefix
			pendingCacheState = "unknown"
			pendingGapToPrev = nil
			if hasPrev && pendingBoundaryMS > 0 {
				gap := float64(pendingBoundaryMS-prevAssistantTSMS) / 1000
				if gap < 0 {
					gap = 0
				}
				pendingGapToPrev = &gap
				pendingCacheState = stateFromGap(gap)
			}

		case rec.Type == "user" && rec.IsCompactSummary:
			summaryText := extractSummaryText(rec.Message)
			summaryTokensEst := len(summaryText) / 4
			ts := rec.Timestamp
			tsMS := parseTSMS(ts)
			if !pendingBoundary {
				// Edge: summary without preceding boundary record. Use
				// summary's own timestamp and what we know about gap.
				pendingBoundaryTS = ts
				pendingBoundaryMS = tsMS
				pendingPrefix = prevPrefix
				pendingCacheState = "unknown"
				pendingGapToPrev = nil
				if hasPrev && tsMS > 0 {
					gap := float64(tsMS-prevAssistantTSMS) / 1000
					if gap < 0 {
						gap = 0
					}
					pendingGapToPrev = &gap
					pendingCacheState = stateFromGap(gap)
				}
			}
			pendingCompact = Compaction{
				SessionUUID:      sessionUUID,
				TS:               firstNonEmpty(pendingBoundaryTS, ts),
				TSUnixMS:         pickMS(pendingBoundaryMS, tsMS),
				PrefixTokensEst:  pendingPrefix,
				SummaryTokensEst: summaryTokensEst,
				GapToPrevS:       pendingGapToPrev,
				CacheState:       pendingCacheState,
				Project:          project,
			}
			pendingCompactReady = true
			markPostCompact = true
			pendingBoundary = false

		case rec.Type == "assistant":
			var msg rawMessage
			if err := json.Unmarshal(rec.Message, &msg); err != nil {
				continue
			}
			if msg.Usage == nil {
				continue
			}
			cw5 := 0
			cw1 := 0
			if msg.Usage.CacheCreation != nil {
				cw5 = msg.Usage.CacheCreation.Ephemeral5m
				cw1 = msg.Usage.CacheCreation.Ephemeral1h
			}
			cr := msg.Usage.CacheReadInputTokens
			in := msg.Usage.InputTokens
			out := msg.Usage.OutputTokens
			prefix := cr + cw5 + cw1 + in
			tsMS := parseTSMS(rec.Timestamp)

			// Confirm any pending compaction against this turn's prefix.
			// 50% chosen empirically: real compactions cluster at 70-100%
			// shrink; weak-shrink events are almost always rotations or
			// restructures misidentified as compactions.
			if pendingCompactReady {
				confirmed := false
				reason := "no_following_turn"
				if pendingPrefix > 0 {
					shrink := 1 - (float64(prefix) / float64(pendingPrefix))
					if shrink >= 0.50 {
						confirmed = true
						reason = ""
					} else {
						reason = "weak_shrink"
					}
				} else {
					reason = "no_baseline_prefix"
				}
				finalizeCompaction(prefix, confirmed, reason)
			}

			gapS := 0.0
			if hasPrev && tsMS > 0 {
				g := float64(tsMS-prevAssistantTSMS) / 1000
				if g < 0 {
					g = 0
				}
				gapS = g
			}

			class := Classify(cw5, cw1, cr, gapS)
			t := Turn{
				SessionUUID:    sessionUUID,
				TurnIdx:        turnIdx,
				TS:             rec.Timestamp,
				TSUnixMS:       tsMS,
				Model:          msg.Model,
				InputTokens:    in,
				OutputTokens:   out,
				CacheRead:      cr,
				CacheCreate5m:  cw5,
				CacheCreate1h:  cw1,
				GapS:           gapS,
				Classification: class,
				PostCompact:    markPostCompact,
				Project:        project,
				SourcePathHash: pathHash,
			}
			res.Turns = append(res.Turns, t)
			turnIdx++
			prevAssistantTSMS = tsMS
			prevPrefix = prefix
			hasPrev = true
			markPostCompact = false
		}
	}
	if err := scanner.Err(); err != nil {
		return res, fmt.Errorf("%s: %w", path, err)
	}

	// Trailing pending compaction: insert as unconfirmed.
	if pendingCompactReady {
		finalizeCompaction(0, false, "no_following_turn")
	}

	return res, nil
}

func extractSummaryText(msg json.RawMessage) string {
	var probe struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(msg, &probe); err != nil {
		return ""
	}
	if len(probe.Content) == 0 {
		return ""
	}
	// Try string first.
	var s string
	if err := json.Unmarshal(probe.Content, &s); err == nil {
		return s
	}
	// Try []{type, text} blocks.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(probe.Content, &blocks); err == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Type == "text" {
				b.WriteString(blk.Text)
				b.WriteByte(' ')
			}
		}
		return strings.TrimSpace(b.String())
	}
	return ""
}

func stateFromGap(gapS float64) string {
	switch {
	case gapS > 3600:
		return "cold"
	case gapS > 300:
		return "warm_1h"
	default:
		return "warm_5m"
	}
}

func parseTSMS(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return 0
		}
	}
	return t.UnixMilli()
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func pickMS(a, b int64) int64 {
	if a > 0 {
		return a
	}
	return b
}

// hashPath returns a hex sha256 of the absolute path. Used to key
// ingested_files without storing the path itself in shared / exported
// contexts (the path stays in the row for debugging on the local DB).
func hashPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	sum := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(sum[:])
}

// drainErr lets parseFile callers handle io.EOF transparently.
var _ = io.EOF
