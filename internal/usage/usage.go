// Package usage drives Claude Code's /usage TUI panel in a pty, captures the
// rendered output, strips ANSI, and parses out the per-bucket percentages and
// reset hints. The scraper is faithful to the Python POC's behaviour but
// returns structured Go types.
package usage

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Bucket is one row from the /usage panel.
type Bucket struct {
	Label    string `json:"label"`
	Pct      int    `json:"pct"`
	ResetRaw string `json:"reset_raw,omitempty"`
}

// Result is everything we know after scraping.
type Result struct {
	OK         bool      `json:"ok"`
	FetchedAt  time.Time `json:"fetched_at"`
	ElapsedS   float64   `json:"elapsed_s"`
	Buckets    []Bucket  `json:"buckets"`
	Raw        string    `json:"raw"` // tail of the cleaned terminal output (debug)
	SessionPct *int      `json:"session_pct,omitempty"`
	WeekAllPct *int      `json:"week_all_pct,omitempty"`
}

// Options configures Fetch.
type Options struct {
	ClaudeBinary string        // empty => "claude" on PATH
	Timeout      time.Duration // 0 => 22s
}

// Fetch spawns Claude Code, types `/usage`, captures and parses the panel.
// Returns the (partially-populated) Result alongside any error so callers can
// persist a failed attempt for debugging.
func Fetch(ctx context.Context, opts Options) (Result, error) {
	if opts.ClaudeBinary == "" {
		opts.ClaudeBinary = "claude"
	}
	if opts.Timeout == 0 {
		opts.Timeout = 22 * time.Second
	}

	t0 := time.Now()
	rawBytes, err := drive(ctx, opts)
	elapsed := time.Since(t0).Seconds()
	cleaned := stripANSI(rawBytes)

	res := Result{
		FetchedAt: time.Now().UTC(),
		ElapsedS:  round2(elapsed),
		Raw:       tail(cleaned, 3000),
	}
	if err != nil {
		return res, err
	}
	res = parseInto(res, cleaned)
	res.OK = len(res.Buckets) > 0
	return res, nil
}

var ansiRe = regexp.MustCompile(strings.Join([]string{
	`\x1b\[[0-?]*[ -/]*[@-~]`,    // CSI sequences
	`\x1b\][^\x07]*\x07`,         // OSC ending in BEL
	`\x1b[PX^_].*?\x1b\\`,        // DCS/SOS/PM/APC ending in ST
	`\x1b[()][AB012]`,            // charset designation
	`\x1b[=>]`,                   // app keypad mode
	`\x1b[78]`,                   // save / restore cursor (ESC 7, ESC 8)
	`\x1bM`,                      // reverse index
	`\x1b\[\?[0-9;]*[a-zA-Z]`,    // private mode
}, "|"))

func stripANSI(b []byte) string {
	return ansiRe.ReplaceAllString(string(b), "")
}

var (
	// pctRe captures the bucket label and percentage. The label-character class
	// is permissive because the TUI uses cursor positioning rather than
	// whitespace, producing run-together text like "Currentweek(allmodels)".
	pctRe    = regexp.MustCompile(`(?i)(Current\s*(?:session|week\s*\(?[^)]*\)?))\s*[\s▀-▟]*?(\d+)\s*%\s*used`)
	// resetRe permits zero whitespace after "Resets" — actual TUI output is
	// often "Resets2am(Europe/Copenhagen)" with no space.
	resetRe  = regexp.MustCompile(`(?i)Resets?\s*([A-Za-z0-9: ,]+?)\s*\([^)]+\)`)
	spacesRe = regexp.MustCompile(`\s+`)
)

func parseInto(res Result, text string) Result {
	pcts := pctRe.FindAllStringSubmatch(text, -1)
	resets := resetRe.FindAllStringSubmatch(text, -1)

	for i, m := range pcts {
		label := strings.TrimSpace(spacesRe.ReplaceAllString(m[1], " "))
		pct, _ := strconv.Atoi(m[2])
		var rs string
		if i < len(resets) {
			rs = strings.TrimSpace(resets[i][1])
		}
		res.Buckets = append(res.Buckets, Bucket{Label: label, Pct: pct, ResetRaw: rs})
	}

	for i := range res.Buckets {
		l := strings.ToLower(res.Buckets[i].Label)
		if res.SessionPct == nil && strings.Contains(l, "session") {
			v := res.Buckets[i].Pct
			res.SessionPct = &v
		}
		if res.WeekAllPct == nil && strings.Contains(l, "week") &&
			(strings.Contains(l, "all") || !strings.Contains(l, "(")) {
			v := res.Buckets[i].Pct
			res.WeekAllPct = &v
		}
	}
	return res
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func round2(f float64) float64 {
	return float64(int(f*100+0.5)) / 100
}
