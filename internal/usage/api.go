package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/version"
)

// The usage endpoint is the one Claude Code's own /usage panel renders from,
// and the one claude-swap polls. It answers a GET with the account's OAuth
// access token, which Claude Code keeps in the config dir's .credentials.json.
//
// Reading it directly costs one small HTTPS request instead of spawning a
// whole claude in a pty, and returns exact reset instants instead of wall
// clock strings we have to parse in the panel's timezone. It is not a
// documented API, so the pty scrape stays behind it as the fallback.
const (
	DefaultAPIURL = "https://api.anthropic.com/api/oauth/usage"
	oauthBeta     = "oauth-2025-04-20"
)

// Sources a Result can come from, recorded per observation.
const (
	SourceAPI = "api"
	SourcePTY = "pty"
)

// expiryMargin treats a token this close to its expiry as already expired.
// A request that races the expiry gets a 401 we would then have to explain.
const expiryMargin = time.Minute

// ErrNoCredentials means the dir has no readable OAuth access token on disk.
// On macOS Claude Code keeps it in the Keychain, so this is the normal state
// there, not a fault.
var ErrNoCredentials = errors.New("no OAuth access token on disk")

// ErrTokenExpired means the stored access token has expired. Bloodhound never
// refreshes it: the refresh token rotates on use, and Claude Code, which owns
// the file, would be left holding a dead one. Running claude (the pty path)
// refreshes it as a side effect, so the next API poll finds a fresh token.
var ErrTokenExpired = errors.New("stored OAuth access token has expired")

// APIError is a non-200 answer from the usage endpoint.
type APIError struct {
	Status     int
	RetryAfter time.Duration // zero when absent, or when the server sent 0
	Body       string        // truncated, for the log
}

func (e *APIError) Error() string {
	if e.Status == http.StatusTooManyRequests {
		return fmt.Sprintf("usage endpoint rate limited (http 429, retry-after %s)", e.RetryAfter)
	}
	return fmt.Sprintf("usage endpoint answered http %d: %s", e.Status, e.Body)
}

// APIOptions configures FetchAPI.
type APIOptions struct {
	// ConfigDir is the Claude Code config dir whose login to use.
	ConfigDir string
	// URL overrides DefaultAPIURL (tests).
	URL string
	// Client overrides the HTTP client. Its timeout, if any, applies on top
	// of Timeout.
	Client *http.Client
	// Timeout bounds the request. 0 => 10s.
	Timeout time.Duration
	// Now overrides the clock used for the expiry check (tests).
	Now func() time.Time
}

// CredentialsFile is where Claude Code keeps dir's OAuth tokens on Linux and
// Windows. Unlike the state file it lives inside the dir even for ~/.claude.
func CredentialsFile(dir string) string {
	return filepath.Join(dir, ".credentials.json")
}

type credentialsFile struct {
	ClaudeAiOauth *struct {
		AccessToken string `json:"accessToken"`
		ExpiresAt   int64  `json:"expiresAt"` // unix ms
	} `json:"claudeAiOauth"`
}

// readAccessToken returns dir's access token, or ErrNoCredentials /
// ErrTokenExpired.
func readAccessToken(dir string, now time.Time) (string, error) {
	data, err := os.ReadFile(CredentialsFile(dir))
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNoCredentials
	}
	if err != nil {
		return "", err
	}
	var cf credentialsFile
	if err := json.Unmarshal(data, &cf); err != nil {
		// Claude Code rewrites this file on every refresh. A torn read is
		// not worth more than falling back for one poll.
		return "", fmt.Errorf("parse %s: %w", CredentialsFile(dir), err)
	}
	if cf.ClaudeAiOauth == nil || cf.ClaudeAiOauth.AccessToken == "" {
		return "", ErrNoCredentials
	}
	if exp := cf.ClaudeAiOauth.ExpiresAt; exp > 0 && now.Add(expiryMargin).UnixMilli() >= exp {
		return "", ErrTokenExpired
	}
	return cf.ClaudeAiOauth.AccessToken, nil
}

// apiWindow is one utilization window in the endpoint's answer. The endpoint
// carries many more keys (per-model windows, extra usage, a breakdown by
// surface); the raw body is kept in raw_dumps so nothing is lost, but only
// the two windows the panel shows are load-bearing here.
type apiWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

type apiResponse struct {
	FiveHour *apiWindow `json:"five_hour"`
	SevenDay *apiWindow `json:"seven_day"`
}

// FetchAPI reads dir's usage from the usage endpoint. Like Fetch it returns
// a populated Result alongside any error, so a failed attempt can still be
// logged; unlike Fetch a failure here is normally answered by falling back
// to the pty (see Collect) rather than recorded.
func FetchAPI(ctx context.Context, opts APIOptions) (Result, error) {
	if opts.URL == "" {
		opts.URL = DefaultAPIURL
	}
	if opts.Client == nil {
		opts.Client = http.DefaultClient
	}
	if opts.Timeout == 0 {
		opts.Timeout = 10 * time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	t0 := time.Now()
	res := Result{Source: SourceAPI}
	finish := func() {
		res.FetchedAt = opts.Now().UTC()
		res.ElapsedS = round2(time.Since(t0).Seconds())
	}

	token, err := readAccessToken(opts.ConfigDir, opts.Now())
	if err != nil {
		finish()
		return res, err
	}

	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.URL, nil)
	if err != nil {
		finish()
		return res, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", oauthBeta)
	req.Header.Set("User-Agent", "claude-code-bloodhound/"+version.Version)
	req.Header.Set("Accept", "application/json")

	resp, err := opts.Client.Do(req)
	if err != nil {
		finish()
		return res, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	finish()
	if err != nil {
		return res, err
	}
	res.Raw = string(body)
	res.RawFull = res.Raw

	if resp.StatusCode != http.StatusOK {
		return res, &APIError{
			Status:     resp.StatusCode,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			Body:       tail(strings.TrimSpace(string(body)), 200),
		}
	}

	var ar apiResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		return res, fmt.Errorf("decode usage response: %w", err)
	}
	res.Extracted = Extracted{Values: map[string]any{}}
	applyWindow(&res, ar.FiveHour, "session", &res.SessionPct, &res.SessionResetAt, &res.SessionResetRaw)
	applyWindow(&res, ar.SevenDay, "week", &res.WeekPct, &res.WeekResetAt, &res.WeekResetRaw)
	res.OK = len(res.Extracted.Missing) == 0
	if !res.OK {
		// A 200 without the windows means the response shape moved. That is
		// a reason to fall back, not a reading of zero.
		return res, fmt.Errorf("usage response lacks %v", res.Extracted.Missing)
	}
	return res, nil
}

// applyWindow copies one window onto res, mirroring what the extractor would
// have reported so the Debug page reads the same for either source.
func applyWindow(res *Result, w *apiWindow, name string, pct **int, resetAt **time.Time, resetRaw *string) {
	if w == nil || w.Utilization == nil {
		res.Extracted.Missing = append(res.Extracted.Missing, name+"_pct")
		return
	}
	// The panel shows whole percentages; the endpoint sends a float that has
	// so far always been whole. Rounding keeps the two sources on one scale.
	v := int(math.Round(*w.Utilization))
	*pct = &v
	res.Extracted.Values[name+"_pct"] = v
	if w.ResetsAt == "" {
		return
	}
	*resetRaw = w.ResetsAt
	res.Extracted.Values[name+"_reset"] = w.ResetsAt
	if t, err := time.Parse(time.RFC3339Nano, w.ResetsAt); err == nil {
		// The endpoint stamps resets a fraction of a second off the minute.
		// The panel's parsed resets are whole minutes, so rounding keeps a
		// reset reading as unmoved when the source switches.
		t = t.UTC().Round(time.Minute)
		*resetAt = &t
	}
}

func parseRetryAfter(s string) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil && n > 0 {
		return time.Duration(n * float64(time.Second))
	}
	if t, err := http.ParseTime(s); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
