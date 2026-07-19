package costweight

import (
	"database/sql"
	"math"
	"testing"

	_ "modernc.org/sqlite"
)

func TestMultiplierTiers(t *testing.T) {
	// Fable is twice Opus; Sonnet is 0.6x; Haiku 0.2x. These ratios are
	// what make a per-model page meaningful, so pin them.
	if got := Multiplier("claude-fable-5"); got != 2.0 {
		t.Errorf("fable-5 = %g, want 2.0", got)
	}
	if got := Multiplier("claude-opus-4-8"); got != 1.0 {
		t.Errorf("opus-4-8 = %g, want 1.0", got)
	}
	if got := Multiplier("claude-sonnet-5"); got != 0.6 {
		t.Errorf("sonnet-5 = %g, want 0.6", got)
	}
}

func TestUnknownModelFallsBackToOpusTier(t *testing.T) {
	// A model we have not tabulated yet (or the "<synthetic>" pseudo-model
	// the ingester emits) must not silently weigh zero — under-counting is
	// worse than over-counting for limit prediction.
	for _, m := range []string{"claude-opus-9-9", "<synthetic>", ""} {
		if got := Multiplier(m); got != DefaultMultiplier {
			t.Errorf("Multiplier(%q) = %g, want %g", m, got, DefaultMultiplier)
		}
		if Known(m) {
			t.Errorf("Known(%q) = true, want false", m)
		}
	}
}

func TestCWAppliesModelFactor(t *testing.T) {
	// Same tokens, different models: cost must scale by the multiplier.
	const in, out, cr, c5, c1 = 1000, 100, 5000, 200, 0
	opus := CW("claude-opus-4-8", in, out, cr, c5, c1)
	fable := CW("claude-fable-5", in, out, cr, c5, c1)
	if math.Abs(fable-2*opus) > 1e-9 {
		t.Errorf("fable = %g, want 2x opus (%g)", fable, 2*opus)
	}
	// Token-type weighting still holds: 1000*1 + 100*5 + 5000*0.1 + 200*1.25 = 2250
	if math.Abs(opus-2250) > 1e-9 {
		t.Errorf("opus = %g, want 2250", opus)
	}
}

// TestSQLExprMatchesGo is the reason this package exists: the aggregator
// weights in Go, the API surfaces weight in SQL, and a divergence between
// them would silently corrupt calibration. Evaluate both over the same
// rows and require agreement.
func TestSQLExprMatchesGo(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE turns (
		model TEXT, input_tokens INT, output_tokens INT,
		cache_read INT, cache_create_5m INT, cache_create_1h INT)`); err != nil {
		t.Fatal(err)
	}

	type row struct {
		model               string
		in, out, cr, c5, c1 int64
	}
	rows := []row{
		{"claude-opus-4-8", 1000, 100, 5000, 200, 0},
		{"claude-fable-5", 900, 250, 12000, 0, 400},
		{"claude-sonnet-5", 400, 50, 800, 30, 0},
		{"claude-haiku-4-5", 100, 10, 0, 0, 0},
		{"some-future-model", 700, 80, 300, 10, 20}, // exercises the ELSE branch
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO turns VALUES (?,?,?,?,?,?)`,
			r.model, r.in, r.out, r.cr, r.c5, r.c1); err != nil {
			t.Fatal(err)
		}
	}

	for _, r := range rows {
		var gotSQL float64
		if err := db.QueryRow(
			`SELECT `+SQLExpr()+` FROM turns WHERE model = ?`, r.model,
		).Scan(&gotSQL); err != nil {
			t.Fatalf("%s: %v", r.model, err)
		}
		wantGo := CW(r.model, r.in, r.out, r.cr, r.c5, r.c1)
		if math.Abs(gotSQL-wantGo) > 1e-6 {
			t.Errorf("%s: SQL = %g, Go = %g", r.model, gotSQL, wantGo)
		}
	}
}
