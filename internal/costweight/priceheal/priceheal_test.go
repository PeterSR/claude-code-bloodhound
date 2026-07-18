package priceheal

import (
	"testing"
	"time"
)

func TestExtractQuote(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		wantOK  bool
		wantIn  float64
		wantErr string
	}{
		{"pure json", `{"input": 3, "output": 15, "source": "https://x"}`, true, 3, ""},
		{"trailing prose", `{"input": 3, "output": 15, "source": "https://x"} — from the docs`, true, 3, ""},
		{"leading prose", `Here is the price: {"input": 7, "output": 35, "source": "https://x"}`, true, 7, ""},
		{"markdown fence", "```json\n{\"input\": 1, \"output\": 5, \"source\": \"https://x\"}\n```", true, 1, ""},
		{"error object", `{"error": "model retired"}`, true, 0, "model retired"},
		{"no json", `I could not find anything.`, false, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q, ok := extractQuote(c.text)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if q.Input != c.wantIn {
				t.Errorf("input = %g, want %g", q.Input, c.wantIn)
			}
			if q.Error != c.wantErr {
				t.Errorf("error = %q, want %q", q.Error, c.wantErr)
			}
		})
	}
}

func TestPlausible(t *testing.T) {
	ok := modelQuote{Input: 3, Output: 15, Source: "https://anthropic.com/pricing"}
	if err := plausible(ok); err != nil {
		t.Errorf("valid quote rejected: %v", err)
	}
	bad := []modelQuote{
		{Input: 0, Output: 15, Source: "https://x"},         // zero input
		{Input: 3, Output: 0, Source: "https://x"},          // zero output
		{Input: 5000, Output: 15, Source: "https://x"},      // absurd
		{Input: 3, Output: 15, Source: ""},                  // no source
		{Input: 3, Output: 15, Source: "anthropic-pricing"}, // not a URL
	}
	for i, q := range bad {
		if err := plausible(q); err == nil {
			t.Errorf("case %d: expected rejection of %+v", i, q)
		}
	}
}

func TestGatePerModelBackoff(t *testing.T) {
	g := NewGate()
	clock := time.Unix(1_000_000, 0)
	g.now = func() time.Time { return clock }

	// Fail model A up to the limit.
	for i := 0; i < g.maxFails; i++ {
		if err := g.tryAcquire("A", false); err != nil {
			t.Fatalf("attempt %d: unexpected %v", i, err)
		}
		g.release("A", false)
	}
	// A is now cooling down...
	if err := g.tryAcquire("A", false); err != ErrCoolingDown {
		t.Errorf("A after %d fails: err = %v, want cooling down", g.maxFails, err)
	}
	// ...but a different model is unaffected.
	if err := g.tryAcquire("B", false); err != nil {
		t.Errorf("B should not be blocked by A's failures: %v", err)
	}
	g.release("B", true)

	// Force bypasses A's cool-down.
	if err := g.tryAcquire("A", true); err != nil {
		t.Errorf("force should bypass cool-down: %v", err)
	}
	g.release("A", true) // success clears backoff
	if g.Backoff("A") {
		t.Error("success should clear A's backoff")
	}

	// After the cool-down elapses, A is allowed again even without force.
	for i := 0; i < g.maxFails; i++ {
		_ = g.tryAcquire("A", false)
		g.release("A", false)
	}
	clock = clock.Add(g.cooldown + time.Minute)
	if err := g.tryAcquire("A", false); err != nil {
		t.Errorf("A after cool-down elapsed: %v", err)
	}
	g.release("A", false)
}

func TestGateMutexIsGlobal(t *testing.T) {
	g := NewGate()
	if err := g.tryAcquire("A", false); err != nil {
		t.Fatal(err)
	}
	// Even a different model can't start while one heal holds the slot.
	if err := g.tryAcquire("B", false); err != ErrAlreadyRunning {
		t.Errorf("second heal: err = %v, want already running", err)
	}
	// Even force can't bypass the mutex.
	if err := g.tryAcquire("B", true); err != ErrAlreadyRunning {
		t.Errorf("forced second heal: err = %v, want already running", err)
	}
	g.release("A", true)
	if err := g.tryAcquire("B", false); err != nil {
		t.Errorf("after release: %v", err)
	}
	g.release("B", true)
}
