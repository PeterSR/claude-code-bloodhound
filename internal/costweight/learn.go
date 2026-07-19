package costweight

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
)

// Learning each model's real rate-limit weight from local data.
//
// The idea: if two models were weighted identically by the rate limiter,
// then spending the same number of type-weighted tokens on either would
// move the /usage percentage by the same amount. So for windows dominated
// by a single model, measure "type-weighted tokens per 1% consumed" and
// compare across models. A model that burns percentage on fewer tokens is
// weighted more heavily.
//
// Three things make this harder than it sounds, and each is defended
// against below:
//
//  1. Circularity. The estimate must never read cost_weighted_tokens,
//     because that column already has the model multiplier baked in —
//     feeding it back would just re-derive the input. We weight by token
//     type only (TypeExpr) and let the model factor fall out as the
//     unknown.
//
//  2. Quantization. /usage reports whole percents, so a 1-point move is
//     mostly rounding noise. Only windows moving at least MinDeltaPct are
//     used.
//
//  3. Era confound — the big one. Model adoption correlates with time: you
//     use one model for a month, then the next. Measured across all
//     history, Opus 4.7 and 4.8 (identically priced, so a correct
//     estimator must return 1.00x) came out 1.75x apart, and a single
//     unchanging model measured 0.30 in one month and 0.72 in another.
//     That drift is the limit regime moving, not the model. So comparisons
//     are restricted to one recent window in which the baseline and the
//     candidate actually coexist.

const (
	// MinDeltaPct is the smallest percentage move a window may show and
	// still be used. Below this, integer rounding dominates the signal.
	MinDeltaPct = 5

	// PurityShare is the fraction of a window's tokens that must belong to
	// one model for the window to be attributed to it. Mixed windows can't
	// separate the models' contributions.
	PurityShare = 0.90

	// MinSamples is the fewest pure windows a model needs before we report
	// a weight. Below this the median is not meaningful.
	MinSamples = 8
)

// Observed is one model's locally-measured weight.
type Observed struct {
	Model string
	// Samples is the number of model-pure windows behind the estimate.
	Samples int
	// TokensPerPct is the median type-weighted tokens per 1% consumed.
	TokensPerPct float64
	// Weight is the measured multiplier relative to the baseline model.
	// Zero when Samples < MinSamples.
	Weight float64
	// ListWeight is the shipped list-price prior for comparison.
	ListWeight float64
	// IsBaseline marks the model the others are measured against.
	IsBaseline bool
}

// Diverges reports whether the measured weight disagrees with the list
// price by more than tol (a fraction, e.g. 0.25 for 25%). Callers use this
// to flag rows in the UI. Always false without enough samples.
func (o Observed) Diverges(tol float64) bool {
	if o.Weight == 0 || o.ListWeight == 0 {
		return false
	}
	return math.Abs(o.Weight-o.ListWeight)/o.ListWeight > tol
}

// LearnOpts scopes the estimate.
type LearnOpts struct {
	// Bucket is "session" or "week". Session is strongly preferred: the
	// week bucket yields almost no model-pure windows, because a week
	// nearly always spans more than one model.
	Bucket string
	// SinceUnixMS restricts to windows after this instant. This is the era
	// guard — widen it and the estimate silently absorbs limit-regime
	// drift as if it were a model difference.
	SinceUnixMS int64
	// Baseline is the model others are measured against. It must be
	// present in the same time range as the candidates, or the comparison
	// is not contemporaneous.
	Baseline string
}

// pointKey identifies one calibration window.
type pointKey struct {
	id    int64
	delta int
}

// Learn measures each model's weight from model-pure calibration windows.
//
// Returns one Observed per model that appears at all, sorted by descending
// sample count. Models below MinSamples are still returned (with Weight 0)
// so the UI can show "not enough data" rather than omitting them silently.
func Learn(ctx context.Context, db *sql.DB, opts LearnOpts) ([]Observed, error) {
	if opts.Bucket == "" {
		opts.Bucket = "session"
	}
	if opts.Baseline == "" {
		opts.Baseline = "claude-opus-4-8"
	}

	// Per calibration window, the type-weighted token total per model.
	// Deliberately TypeExpr and not SQLExpr — see the circularity note.
	rows, err := db.QueryContext(ctx, `
		SELECT cp.id, cp.delta_pct, t.model, SUM(`+TypeExprFor("t")+`) AS tw
		FROM calibration_points cp
		JOIN turns t
		  ON t.ts_unix_ms >  cp.a_ts_unix_ms
		 AND t.ts_unix_ms <= cp.b_ts_unix_ms
		WHERE cp.bucket = ?
		  AND cp.delta_pct >= ?
		  AND cp.b_ts_unix_ms >= ?
		GROUP BY cp.id, t.model
	`, opts.Bucket, MinDeltaPct, opts.SinceUnixMS)
	if err != nil {
		return nil, fmt.Errorf("learn: query: %w", err)
	}
	defer rows.Close()

	perPoint := map[pointKey]map[string]float64{}
	for rows.Next() {
		var (
			id    int64
			delta int
			model string
			tw    float64
		)
		if err := rows.Scan(&id, &delta, &model, &tw); err != nil {
			return nil, fmt.Errorf("learn: scan: %w", err)
		}
		k := pointKey{id: id, delta: delta}
		if perPoint[k] == nil {
			perPoint[k] = map[string]float64{}
		}
		perPoint[k][model] += tw
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("learn: rows: %w", err)
	}

	// Attribute each window to its dominant model, and record that
	// window's tokens-per-percent.
	samples := map[string][]float64{}
	for k, byModel := range perPoint {
		var total float64
		for _, v := range byModel {
			total += v
		}
		if total <= 0 || k.delta <= 0 {
			continue
		}
		for model, v := range byModel {
			if v/total >= PurityShare {
				samples[model] = append(samples[model], total/float64(k.delta))
				break
			}
		}
	}

	// The baseline anchors the scale; without it there is nothing to
	// measure against and every weight would be arbitrary.
	baseTPP, baseOK := median(samples[opts.Baseline])
	if len(samples[opts.Baseline]) < MinSamples {
		baseOK = false
	}

	out := make([]Observed, 0, len(samples))
	for model, s := range samples {
		o := Observed{
			Model:      model,
			Samples:    len(s),
			ListWeight: Multiplier(model),
			IsBaseline: model == opts.Baseline,
		}
		if m, ok := median(s); ok {
			o.TokensPerPct = m
			// Fewer tokens per percent means the model burns the limit
			// faster, so weight is the inverse ratio.
			if baseOK && m > 0 && len(s) >= MinSamples {
				o.Weight = baseTPP / m
			}
		}
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Samples > out[j].Samples })
	return out, nil
}

// ControlError estimates the method's own systematic error using a pair of
// models known to be weighted identically (same list price). A correct
// estimator returns 1.00x between them; whatever it actually returns is
// the noise floor, and no divergence smaller than that floor should be
// believed.
//
// Returns the measured ratio and whether both models had enough samples.
func ControlError(obs []Observed, a, b string) (ratio float64, ok bool) {
	var wa, wb float64
	for _, o := range obs {
		switch o.Model {
		case a:
			wa = o.Weight
			if o.IsBaseline {
				wa = 1.0
			}
		case b:
			wb = o.Weight
			if o.IsBaseline {
				wb = 1.0
			}
		}
	}
	if wa == 0 || wb == 0 {
		return 0, false
	}
	if Multiplier(a) != Multiplier(b) {
		// Not a valid control pair — they are not supposed to match.
		return 0, false
	}
	return wa / wb, true
}

func median(xs []float64) (float64, bool) {
	if len(xs) == 0 {
		return 0, false
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2], true
	}
	return (s[n/2-1] + s[n/2]) / 2, true
}
