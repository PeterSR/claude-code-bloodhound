package usage

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeSources counts calls into each source and answers with fixed outcomes.
type fakeSources struct {
	apiErr, ptyErr     error
	apiCalls, ptyCalls int
}

func (f *fakeSources) opts(mode string, gate *APIGate) CollectOptions {
	return CollectOptions{
		Mode: mode,
		API:  APIOptions{ConfigDir: "/cfg/a"},
		Gate: gate,
		fetchAPI: func(context.Context, APIOptions) (Result, error) {
			f.apiCalls++
			return Result{Source: SourceAPI, OK: f.apiErr == nil}, f.apiErr
		},
		fetchPTY: func(context.Context, Options) (Result, error) {
			f.ptyCalls++
			return Result{Source: SourcePTY, OK: f.ptyErr == nil}, f.ptyErr
		},
	}
}

func TestCollect_AutoPrefersTheAPI(t *testing.T) {
	f := &fakeSources{}
	res, fallback, err := Collect(context.Background(), f.opts(ModeAuto, nil))
	if err != nil || fallback != nil || res.Source != SourceAPI {
		t.Fatalf("res=%q fallback=%v err=%v", res.Source, fallback, err)
	}
	if f.ptyCalls != 0 {
		t.Error("pty driven although the api answered")
	}
}

func TestCollect_AutoFallsBackToThePTY(t *testing.T) {
	for _, apiErr := range []error{ErrNoCredentials, ErrTokenExpired, &APIError{Status: 500}, errors.New("dial tcp: refused")} {
		f := &fakeSources{apiErr: apiErr}
		res, fallback, err := Collect(context.Background(), f.opts("", nil))
		if err != nil || res.Source != SourcePTY {
			t.Errorf("%v: res=%q err=%v", apiErr, res.Source, err)
		}
		if !errors.Is(fallback, apiErr) {
			t.Errorf("fallback reason lost: got %v want %v", fallback, apiErr)
		}
	}
}

func TestCollect_PTYFailureIsTheError(t *testing.T) {
	ptyErr := errors.New("pty start failed")
	f := &fakeSources{apiErr: ErrNoCredentials, ptyErr: ptyErr}
	_, fallback, err := Collect(context.Background(), f.opts(ModeAuto, nil))
	if !errors.Is(err, ptyErr) || !errors.Is(fallback, ErrNoCredentials) {
		t.Fatalf("err=%v fallback=%v", err, fallback)
	}
}

func TestCollect_ExplicitModesNeverFallBack(t *testing.T) {
	f := &fakeSources{apiErr: ErrNoCredentials}
	if _, fb, err := Collect(context.Background(), f.opts(ModeAPI, nil)); !errors.Is(err, ErrNoCredentials) || fb != nil {
		t.Errorf("api mode: err=%v fallback=%v", err, fb)
	}
	if f.ptyCalls != 0 {
		t.Error("api mode drove the pty")
	}

	f = &fakeSources{}
	if res, _, _ := Collect(context.Background(), f.opts(ModePTY, nil)); res.Source != SourcePTY || f.apiCalls != 0 {
		t.Errorf("pty mode: source=%q apiCalls=%d", res.Source, f.apiCalls)
	}
}

func TestCollect_UnknownMode(t *testing.T) {
	f := &fakeSources{}
	if _, _, err := Collect(context.Background(), f.opts("carrier-pigeon", nil)); err == nil {
		t.Fatal("unknown mode accepted")
	}
	if f.apiCalls+f.ptyCalls != 0 {
		t.Error("a source ran for an unknown mode")
	}
}

func TestCollect_CancelledContextDoesNotStartThePTY(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &fakeSources{apiErr: context.Canceled}
	_, _, err := Collect(ctx, f.opts(ModeAuto, nil))
	if !errors.Is(err, context.Canceled) || f.ptyCalls != 0 {
		t.Fatalf("err=%v ptyCalls=%d", err, f.ptyCalls)
	}
}

func TestAPIGate_RateLimitBacksOffThenRecovers(t *testing.T) {
	now := time.Date(2026, 1, 2, 6, 0, 0, 0, time.UTC)
	gate := NewAPIGate()
	gate.now = func() time.Time { return now }

	f := &fakeSources{apiErr: &APIError{Status: 429}}
	if _, fb, _ := Collect(context.Background(), f.opts(ModeAuto, gate)); fb == nil {
		t.Fatal("429 did not fall back")
	}

	// Inside the backoff the api is not asked at all.
	now = now.Add(RateLimitBackoff - time.Minute)
	f.apiErr = nil
	_, fb, _ := Collect(context.Background(), f.opts(ModeAuto, gate))
	if !errors.Is(fb, ErrAPIBackoff) || f.apiCalls != 1 {
		t.Fatalf("backoff not honoured: fallback=%v apiCalls=%d", fb, f.apiCalls)
	}
	// The gate is per dir.
	other := f.opts(ModeAuto, gate)
	other.API.ConfigDir = "/cfg/b"
	if res, _, _ := Collect(context.Background(), other); res.Source != SourceAPI {
		t.Error("a 429 on one dir blocked another")
	}

	// After it, the api is back.
	now = now.Add(2 * time.Minute)
	if res, fb, _ := Collect(context.Background(), f.opts(ModeAuto, gate)); res.Source != SourceAPI || fb != nil {
		t.Fatalf("api not retried after backoff: source=%q fallback=%v", res.Source, fb)
	}
}

func TestAPIGate_LongRetryAfterWins(t *testing.T) {
	now := time.Date(2026, 1, 2, 6, 0, 0, 0, time.UTC)
	gate := NewAPIGate()
	gate.now = func() time.Time { return now }
	gate.note("/d", &APIError{Status: 429, RetryAfter: time.Hour})
	if until, ok := gate.blocked("/d"); !ok || !until.Equal(now.Add(time.Hour)) {
		t.Fatalf("until=%v ok=%v", until, ok)
	}
}

func TestAPIGate_OtherErrorsDoNotBackOff(t *testing.T) {
	gate := NewAPIGate()
	for _, err := range []error{ErrTokenExpired, &APIError{Status: 401}, &APIError{Status: 503}} {
		gate.note("/d", err)
		if _, ok := gate.blocked("/d"); ok {
			t.Errorf("%v set a backoff", err)
		}
	}
}
