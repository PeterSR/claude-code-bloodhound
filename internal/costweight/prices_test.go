package costweight

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBundledTableIsValid(t *testing.T) {
	// The embedded default must always parse and validate; a bad edit to
	// prices_default.json should fail here, not panic in production.
	tbl := bundledTable()
	if err := tbl.Validate(); err != nil {
		t.Fatalf("bundled table invalid: %v", err)
	}
	if tbl.Origin() != PriceOriginDefault {
		t.Errorf("origin = %q, want default", tbl.Origin())
	}
}

func TestMultiplierIsDerivedFromPrices(t *testing.T) {
	tbl := bundledTable()
	// input price over baseline (Opus $5) input price.
	cases := map[string]float64{
		"claude-fable-5":   2.0, // 10/5
		"claude-opus-4-8":  1.0, // 5/5 (baseline)
		"claude-sonnet-5":  0.6, // 3/5
		"claude-haiku-4-5": 0.2, // 1/5
	}
	for model, want := range cases {
		got, known := tbl.Multiplier(model)
		if !known {
			t.Errorf("%s: not known", model)
		}
		if got != want {
			t.Errorf("%s: multiplier = %g, want %g", model, got, want)
		}
	}
}

func TestUnknownModelIsFlaggedNotZero(t *testing.T) {
	tbl := bundledTable()
	got, known := tbl.Multiplier("claude-nonesuch-9")
	if known {
		t.Error("unknown model reported as known")
	}
	if got != DefaultMultiplier {
		t.Errorf("unknown multiplier = %g, want %g (never 0 — under-counting is worse)", got, DefaultMultiplier)
	}
}

func TestBreaksRatioAssumption(t *testing.T) {
	// The whole single-scalar scheme assumes output = 5x input. A model
	// that violates it must be flaggable so the UI can warn.
	tbl := &Table{
		Version:  PriceTableVersion,
		Baseline: "base",
		Models: map[string]ModelPrice{
			"base":    {Input: 5, Output: 25}, // 5:1, fine
			"weird":   {Input: 4, Output: 40}, // 10:1, breaks it
			"missing": {Input: 0, Output: 0},  // no claim
		},
	}
	if tbl.BreaksRatioAssumption("base") {
		t.Error("base prices output at 5x input; should not flag")
	}
	if !tbl.BreaksRatioAssumption("weird") {
		t.Error("weird prices output at 10x input; should flag")
	}
	if tbl.BreaksRatioAssumption("missing") || tbl.BreaksRatioAssumption("absent") {
		t.Error("no claim should be made about unpriced/absent models")
	}
}

func TestValidateRejectsUnusableTables(t *testing.T) {
	bad := []struct {
		name string
		tbl  Table
	}{
		{"wrong version", Table{Version: PriceTableVersion + 1, Baseline: "x",
			Models: map[string]ModelPrice{"x": {Input: 5}}}},
		{"no baseline", Table{Version: PriceTableVersion,
			Models: map[string]ModelPrice{"x": {Input: 5}}}},
		{"baseline absent", Table{Version: PriceTableVersion, Baseline: "y",
			Models: map[string]ModelPrice{"x": {Input: 5}}}},
		{"free baseline", Table{Version: PriceTableVersion, Baseline: "x",
			Models: map[string]ModelPrice{"x": {Input: 0}}}},
	}
	for _, c := range bad {
		if err := c.tbl.Validate(); err == nil {
			t.Errorf("%s: expected validation error", c.name)
		}
	}
}

func TestLoadTableFallsBackOnCorruptOrMissing(t *testing.T) {
	// A corrupt or version-mismatched user file must never break cost
	// weighting — it silently yields the bundled default. This is the one
	// place the price table is deliberately softer than the extractor.
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	bhDir := filepath.Join(dir, "bloodhound")
	if err := os.MkdirAll(bhDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(bhDir, pricesFileName)

	for _, body := range []string{
		`{ this is not json `,
		`{"version": 999, "baseline": "claude-opus-4-8", "models": {}}`,
		`{"version": 1, "baseline": "missing-model", "models": {}}`,
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		tbl, err := LoadTable()
		if err != nil {
			t.Fatalf("LoadTable errored instead of falling back: %v", err)
		}
		if tbl.Origin() != PriceOriginDefault {
			t.Errorf("body %q: origin = %q, want default (fallback)", body, tbl.Origin())
		}
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	tbl := &Table{
		Version:  PriceTableVersion,
		Baseline: "claude-opus-4-8",
		Models: map[string]ModelPrice{
			"claude-opus-4-8": {Input: 5, Output: 25},
			"claude-newmodel": {Input: 7, Output: 35, Source: "https://example/pricing"},
		},
	}
	if err := SaveTable(tbl); err != nil {
		t.Fatal(err)
	}
	got, err := LoadTable()
	if err != nil {
		t.Fatal(err)
	}
	if got.Origin() != PriceOriginUser {
		t.Errorf("origin = %q, want user", got.Origin())
	}
	w, known := got.Multiplier("claude-newmodel")
	if !known || w != 1.4 { // 7/5
		t.Errorf("newmodel multiplier = %g (known=%v), want 1.4", w, known)
	}
	if got.Models["claude-newmodel"].Source != "https://example/pricing" {
		t.Error("source not round-tripped")
	}
	// Leave the cache clean for other tests.
	Reload()
}
