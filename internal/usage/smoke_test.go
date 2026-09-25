//go:build smoke

// Smoke tests against the real world: the real usage endpoint with the real
// token in a real config dir, and a real claude driven in a pty. They exist to
// answer "does this path still work?" for whichever path the daemon is not
// currently exercising (usually the pty, since the api answers first).
//
// Never run by `go test ./...` or CI. Run with `make smoke`, or:
//
//	go test -tags smoke -count=1 -v -run Smoke ./internal/usage/
//	go test -tags smoke -count=1 -v -run Smoke/api ./internal/usage/
//
// BLOODHOUND_SMOKE_DIR picks the config dir (default ~/.claude), and
// BLOODHOUND_SMOKE_CLAUDE the claude binary (default "claude" on PATH).
//
// Neither path touches bloodhound's database. The api subtest spends one
// request of the endpoint's roughly-30-an-hour budget; the pty subtest starts
// claude, which is what every fallback poll does anyway.
package usage

import (
	"context"
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func smokeDir(t *testing.T) string {
	if d := os.Getenv("BLOODHOUND_SMOKE_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(home, ".claude")
}

func TestSmoke(t *testing.T) {
	dir := smokeDir(t)
	t.Logf("config dir: %s", dir)
	var apiRes, ptyRes *Result

	t.Run("api", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		res, err := FetchAPI(ctx, APIOptions{ConfigDir: dir})
		var ae *APIError
		switch {
		case errors.Is(err, ErrNoCredentials), errors.Is(err, ErrTokenExpired):
			// Not a fault of the path: nothing usable on disk right now.
			// Starting claude in that dir refreshes an expired token.
			t.Skipf("no usable token: %v", err)
		case errors.As(err, &ae) && ae.Status == http.StatusTooManyRequests:
			// The endpoint answered and authenticated us; the budget is just
			// spent. That proves less than a 200, so say so rather than pass.
			t.Skipf("rate limited, cannot confirm the response shape now: %v", err)
		case err != nil:
			t.Fatalf("api read failed: %v\nbody: %s", err, res.Raw)
		}
		checkReading(t, res)
		apiRes = &res
	})

	t.Run("pty", func(t *testing.T) {
		bin := os.Getenv("BLOODHOUND_SMOKE_CLAUDE")
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		res, err := Fetch(ctx, Options{ClaudeBinary: bin, ConfigDir: dir})
		if err != nil {
			t.Fatalf("pty drive failed after %.1fs: %v\nscreen tail:\n%s", res.ElapsedS, err, res.Raw)
		}
		if !res.OK {
			t.Fatalf("active extractor (%s) missed %v\npanel captured: %v\nscreen tail:\n%s",
				res.ExtractorOrigin, res.Extracted.Missing, PanelCaptured(res.RawFull), res.Raw)
		}
		checkReading(t, res)
		// The bundled default is what a fresh install and every self-heal
		// fallback start from, so it drifting from the live panel is worth
		// knowing about even while a healed extractor still works.
		if res.ExtractorOrigin != string(OriginDefault) {
			def, err := ParseExtractor(defaultExtractorJSON)
			if err != nil {
				t.Fatalf("bundled extractor: %v", err)
			}
			if miss := def.Apply(res.RawFull).Missing; len(miss) > 0 {
				t.Errorf("bundled default extractor misses %v on the live panel (a healed one is in use)", miss)
			}
		}
		ptyRes = &res
	})

	t.Run("agree", func(t *testing.T) {
		if apiRes == nil || ptyRes == nil {
			t.Skip("needs both sources to have read")
		}
		// Seconds apart, so a busy session can move a point in between.
		if d := absDiff(*apiRes.SessionPct, *ptyRes.SessionPct); d > 1 {
			t.Errorf("session: api %d%% vs pty %d%%", *apiRes.SessionPct, *ptyRes.SessionPct)
		}
		if d := absDiff(*apiRes.WeekPct, *ptyRes.WeekPct); d > 1 {
			t.Errorf("week: api %d%% vs pty %d%%", *apiRes.WeekPct, *ptyRes.WeekPct)
		}
		// The panel only shows resets to the minute (or the hour), in its own
		// timezone, so compare what ParseReset makes of it.
		for _, c := range []struct {
			name    string
			exact   *time.Time
			raw, tz string
		}{
			{"session", apiRes.SessionResetAt, ptyRes.SessionResetRaw, ptyRes.SessionResetTZ},
			{"week", apiRes.WeekResetAt, ptyRes.WeekResetRaw, ptyRes.WeekResetTZ},
		} {
			if c.exact == nil || c.raw == "" {
				continue
			}
			parsed, ok := ParseReset(c.raw, c.tz, ptyRes.FetchedAt)
			if !ok {
				t.Errorf("%s: pty reset %q (%s) does not parse", c.name, c.raw, c.tz)
				continue
			}
			if d := parsed.Sub(*c.exact); math.Abs(d.Minutes()) > 60 {
				t.Errorf("%s reset: api %s vs pty %s", c.name, c.exact.Format(time.RFC3339), parsed.Format(time.RFC3339))
			}
		}
	})
}

// checkReading asserts a reading is plausible, not just present.
func checkReading(t *testing.T, res Result) {
	t.Helper()
	t.Logf("%s: session=%v week=%v resets=%v / %v (%s %s) in %.1fs", res.Source,
		deref(res.SessionPct), deref(res.WeekPct), res.SessionResetAt, res.WeekResetAt,
		res.SessionResetRaw, res.WeekResetRaw, res.ElapsedS)
	if res.SessionPct == nil || res.WeekPct == nil {
		t.Fatalf("missing percentages: session=%v week=%v", res.SessionPct, res.WeekPct)
	}
	// Above 100 happens on extra usage; far above means we read a wrong field.
	for name, p := range map[string]int{"session": *res.SessionPct, "week": *res.WeekPct} {
		if p < 0 || p > 200 {
			t.Errorf("%s pct %d is not a percentage", name, p)
		}
	}
	now := time.Now()
	for name, at := range map[string]*time.Time{"session": res.SessionResetAt, "week": res.WeekResetAt} {
		if at != nil && (at.Before(now.Add(-time.Hour)) || at.After(now.Add(8*24*time.Hour))) {
			t.Errorf("%s reset %s is outside the window it belongs to", name, at)
		}
	}
}

func deref(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func absDiff(a, b int) int {
	if a > b {
		return a - b
	}
	return b - a
}
