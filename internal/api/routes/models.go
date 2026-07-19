package routes

// ModelsResponse backs the Models page, which answers two questions of
// very different confidence and keeps them visibly apart.
//
// Attribution (Models, Days) is arithmetic: every turn records the model
// that served it, so splitting spend by model involves no inference and
// carries no caveats.
//
// Weights is an estimate. It measures what each model actually costs of a
// real limit window, which is not something Anthropic publishes. It only
// works for models used near-exclusively for a stretch, so most rows will
// honestly read "not enough data" forever.
type ModelsResponse struct {
	OK         bool `json:"ok"`
	WindowDays int  `json:"window_days"`

	// Models are the priced models seen in the window, ordered by
	// descending cost-weighted tokens. Their Share sums to 1.
	Models []ModelTotal `json:"models"`

	// Unpriced are models seen in the window but absent from the price
	// table. They carry no cost-weighted share — without a price there is
	// no honest weight — so they are held out of the attribution rather
	// than folded in at a guessed rate. Reported in raw-token terms so the
	// user can see the volume waiting to be priced.
	Unpriced []ModelTotal `json:"unpriced"`

	// Days is the attribution series, one entry per local calendar day.
	Days []ModelDay `json:"days"`

	// TotalCWTokens is the priced window total, the denominator behind
	// Share.
	TotalCWTokens float64 `json:"total_cw_tokens"`

	// Prices describes the active price table so the page can show where
	// its numbers came from.
	Prices PriceTableInfo `json:"prices"`

	Weights WeightsReport `json:"weights"`
}

// PriceTableInfo is the provenance of the active price table.
type PriceTableInfo struct {
	// Origin is "user" (an on-disk override) or "default" (bundled).
	Origin      string `json:"origin"`
	Version     int    `json:"version"`
	Baseline    string `json:"baseline"`
	GeneratedAt string `json:"generated_at,omitempty"`
	GeneratedBy string `json:"generated_by,omitempty"`
	// PricedCount is how many models the table prices.
	PricedCount int `json:"priced_count"`
}

// ModelTotal is one model's share of the window.
type ModelTotal struct {
	Model     string  `json:"model"`
	Turns     int64   `json:"turns"`
	RawTokens int64   `json:"raw_tokens"`
	CWTokens  float64 `json:"cw_tokens"`
	// Share of the priced window's cost-weighted tokens, 0..1. This doubles
	// as share of limit burned: cost-weighted tokens are our model of what
	// moves the percentage, so the two are the same number by construction,
	// not by coincidence. Zero for unpriced models.
	Share float64 `json:"share"`
	// Multiplier is the list-price weight applied to this model.
	Multiplier float64 `json:"multiplier"`
	// Known is false when Multiplier is a fallback guess rather than a
	// tabulated ratio, which the UI flags.
	Known bool `json:"known"`
	// InputPrice / OutputPrice are the per-million-token list prices behind
	// the multiplier, shown so the weight isn't a bare number. Zero for
	// unpriced models.
	InputPrice  float64 `json:"input_price,omitempty"`
	OutputPrice float64 `json:"output_price,omitempty"`
	// BreaksRatio is true when this model prices output at something other
	// than 5x its input — the assumption the single-scalar weighting rests
	// on. When set, the model's weight is an approximation and the UI says
	// so.
	BreaksRatio bool `json:"breaks_ratio,omitempty"`
	// Unverified is true for an auto-found price awaiting corroboration by
	// local calibration. Source is where it was found.
	Unverified bool   `json:"unverified,omitempty"`
	Source     string `json:"source,omitempty"`
}

// ModelDay is one day's spend, split by model.
type ModelDay struct {
	TSUnixMS int64 `json:"ts_unix_ms"`
	// CWTokens is keyed by model name; models absent that day are omitted
	// rather than zero-filled.
	CWTokens map[string]float64 `json:"cw_tokens"`
	TotalCW  float64            `json:"total_cw_tokens"`
}

// WeightsReport is the learned-vs-list comparison and everything needed to
// judge how much to trust it.
type WeightsReport struct {
	Rows []ModelWeight `json:"rows"`
	// Baseline is the model the others are measured against; it is 1.00x
	// by definition.
	Baseline string `json:"baseline"`
	// BaselineOK is false when the baseline itself lacked samples, in
	// which case no row can carry a weight at all.
	BaselineOK bool `json:"baseline_ok"`
	// SinceUnixMS is the era guard. Model adoption correlates with time,
	// so a weight measured across eras silently absorbs limit-regime drift
	// as if it were a property of the model. Widening this window makes
	// the estimate worse, not better, and the UI says so.
	SinceUnixMS int64  `json:"since_unix_ms"`
	Days        int    `json:"days"`
	Bucket      string `json:"bucket"`
	MinSamples  int    `json:"min_samples"`

	// Control is the method's own error bar. Two models that list at the
	// same price must measure 1.00x apart; whatever they do measure is the
	// noise floor, and no divergence smaller than it should be believed.
	// Absent when no same-price pair had enough samples on both sides.
	Control *ControlPair `json:"control,omitempty"`
}

// ModelWeight is one model's measured cost against the baseline.
type ModelWeight struct {
	Model string `json:"model"`
	// Samples is the number of model-pure limit windows behind the
	// estimate.
	Samples int `json:"samples"`
	// TokensPerPct is the median type-weighted tokens per 1% consumed.
	// Deliberately type-weighted and not cost-weighted: cost-weighted
	// already has the model multiplier in it, so feeding it back would
	// just re-derive the input.
	TokensPerPct float64 `json:"tokens_per_pct"`
	// Weight is the measured multiplier. Zero means not enough data.
	Weight float64 `json:"weight"`
	// ListWeight is the shipped list-price prior.
	ListWeight float64 `json:"list_weight"`
	IsBaseline bool    `json:"is_baseline"`
	// Diverges is true when the measurement disagrees with list price by
	// more than the divergence tolerance.
	Diverges bool `json:"diverges"`
}

// ControlPair is a measurement between two identically-priced models.
type ControlPair struct {
	A     string  `json:"a"`
	B     string  `json:"b"`
	Ratio float64 `json:"ratio"` // 1.00 would be a perfect estimator
}
