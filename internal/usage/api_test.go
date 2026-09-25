package usage

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// fakeUsageBody mirrors the endpoint's shape, trimmed to the keys that matter
// plus a couple that don't, so decoding is shown to ignore them. Synthetic
// numbers only.
const fakeUsageBody = `{
  "five_hour": {"utilization": 42.0, "resets_at": "2026-01-02T08:00:00.165763+00:00", "locked_reason": null},
  "seven_day": {"utilization": 61.4, "resets_at": "2026-01-05T01:59:59.912345+00:00"},
  "seven_day_sonnet": null,
  "limits": [{"kind": "session", "percent": 42}]
}`

var testNow = time.Date(2026, 1, 2, 6, 0, 0, 0, time.UTC)

func writeCreds(t *testing.T, token string, expiresAt time.Time) string {
	t.Helper()
	dir := t.TempDir()
	body := `{"claudeAiOauth":{"accessToken":"` + token + `","refreshToken":"rt-synthetic","expiresAt":` +
		strconv.FormatInt(expiresAt.UnixMilli(), 10) + `}}`
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func apiOpts(dir, url string) APIOptions {
	return APIOptions{ConfigDir: dir, URL: url, Now: func() time.Time { return testNow }}
}

func TestFetchAPI_ParsesWindowsAndSendsToken(t *testing.T) {
	var gotAuth, gotBeta, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotBeta, gotUA = r.Header.Get("Authorization"), r.Header.Get("anthropic-beta"), r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(fakeUsageBody))
	}))
	defer srv.Close()
	dir := writeCreds(t, "tok-abc", testNow.Add(time.Hour))

	res, err := FetchAPI(context.Background(), apiOpts(dir, srv.URL))
	if err != nil {
		t.Fatalf("FetchAPI: %v", err)
	}
	if gotAuth != "Bearer tok-abc" || gotBeta != oauthBeta || gotUA == "" {
		t.Errorf("headers: auth=%q beta=%q ua=%q", gotAuth, gotBeta, gotUA)
	}
	if !res.OK || res.Source != SourceAPI {
		t.Errorf("OK=%v Source=%q", res.OK, res.Source)
	}
	if res.SessionPct == nil || *res.SessionPct != 42 {
		t.Errorf("session pct: %v", res.SessionPct)
	}
	if res.WeekPct == nil || *res.WeekPct != 61 {
		t.Errorf("week pct (61.4 rounds to 61): %v", res.WeekPct)
	}
	// Resets land on whole minutes whichever side of the minute they were
	// stamped, so a switch between sources does not read as a moved reset.
	if want := time.Date(2026, 1, 2, 8, 0, 0, 0, time.UTC); res.SessionResetAt == nil || !res.SessionResetAt.Equal(want) {
		t.Errorf("session reset: got %v want %v", res.SessionResetAt, want)
	}
	if want := time.Date(2026, 1, 5, 2, 0, 0, 0, time.UTC); res.WeekResetAt == nil || !res.WeekResetAt.Equal(want) {
		t.Errorf("week reset: got %v want %v", res.WeekResetAt, want)
	}
	if res.Raw != fakeUsageBody {
		t.Error("raw body not kept for raw_dumps")
	}
	if res.Extracted.Values["session_pct"] != 42 || len(res.Extracted.Missing) != 0 {
		t.Errorf("extracted mirror: %+v", res.Extracted)
	}
}

func TestFetchAPI_RateLimitCarriesRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error"}}`))
	}))
	defer srv.Close()
	dir := writeCreds(t, "tok", testNow.Add(time.Hour))

	_, err := FetchAPI(context.Background(), apiOpts(dir, srv.URL))
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 429 || ae.RetryAfter != 2*time.Minute {
		t.Fatalf("want 429 with 2m retry-after, got %v", err)
	}
}

func TestFetchAPI_MissingWindowIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"five_hour": {"utilization": 3.0}, "seven_day": null}`))
	}))
	defer srv.Close()
	dir := writeCreds(t, "tok", testNow.Add(time.Hour))

	res, err := FetchAPI(context.Background(), apiOpts(dir, srv.URL))
	if err == nil || res.OK {
		t.Fatalf("a response without seven_day must not pass as a reading: ok=%v err=%v", res.OK, err)
	}
	if len(res.Extracted.Missing) != 1 || res.Extracted.Missing[0] != "week_pct" {
		t.Errorf("missing: %v", res.Extracted.Missing)
	}
}

func TestFetchAPI_NeverSendsAnExpiredToken(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer srv.Close()
	// Inside the margin counts as expired.
	dir := writeCreds(t, "tok", testNow.Add(30*time.Second))

	_, err := FetchAPI(context.Background(), apiOpts(dir, srv.URL))
	if !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("want ErrTokenExpired, got %v", err)
	}
	if called {
		t.Error("an expired token reached the server")
	}
}

func TestFetchAPI_NoCredentialsFile(t *testing.T) {
	_, err := FetchAPI(context.Background(), apiOpts(t.TempDir(), "http://127.0.0.1:1"))
	if !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("want ErrNoCredentials, got %v", err)
	}
}

func TestFetchAPI_CredentialsWithoutOAuth(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{"mcpOAuth":{}}`), 0o600)
	_, err := FetchAPI(context.Background(), apiOpts(dir, "http://127.0.0.1:1"))
	if !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("want ErrNoCredentials, got %v", err)
	}
}

func TestParseRetryAfter(t *testing.T) {
	for in, want := range map[string]time.Duration{"": 0, "0": 0, "30": 30 * time.Second, "junk": 0, "-5": 0} {
		if got := parseRetryAfter(in); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}
