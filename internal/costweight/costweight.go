// Package costweight converts a turn's raw token columns into
// cost-weighted tokens — the common currency every downstream surface
// (buckets, calibration, leaks, session insight) compares usage in.
//
// Weighting has two independent factors:
//
//  1. Token type. Anthropic prices cache reads, cache writes, input and
//     output differently, so each column carries its own multiplier.
//  2. Model. A Fable-5 token costs more than an Opus token, which costs
//     more than a Sonnet token.
//
// The two factors compose as a plain product because every current model
// prices output at exactly 5x its own input ($10/$50, $5/$25, $3/$15,
// $1/$5). That shared ratio is what lets one scalar per model sit on top
// of a single shared token-type formula; if a future model breaks the 5:1
// ratio, this package has to grow a per-model token-type table instead.
//
// Go and SQL callers both derive their weighting here so the two can
// never drift.
package costweight

import (
	"fmt"
	"strings"
)

// Token-type weights, relative to one input token on the same model.
// Mirrors Anthropic's published cache-read / cache-write / output
// multipliers. Exported: these are facts about Anthropic's published
// pricing, not internals of this package, and callers outside it (the
// cold-prefix estimate in sessioninsight) need the cache-write pair
// specifically rather than re-deriving them.
const (
	WInput        = 1.0
	WOutput       = 5.0
	WCacheRead    = 0.1
	WCacheWrite5m = 1.25
	WCacheWrite1h = 2.0
)

// CacheTTL1hSeconds / CacheTTL5mSeconds are the two cache TTLs Anthropic
// supports, in seconds. Callers that classify a turn's or session's cache
// state (sessioninsight.ForSession) compare an observed age against these
// rather than carrying their own copy of the TTL numbers.
const (
	CacheTTL1hSeconds int64 = 3600
	CacheTTL5mSeconds int64 = 300
)

// CacheWriteWeight returns the cache-write weight for the given TTL, in
// seconds. Only 3600 (1h) and 300 (5m) are real Anthropic TTLs; any other
// input (a TTL we failed to classify, or a future value this package
// doesn't know about yet) falls back to WCacheWrite5m. 5m is Anthropic's
// own default cache TTL, applied whenever a request does not opt into the
// 1h extended-cache beta, so an unrecognized TTL is more likely a missing
// or unclassified default than a misread opt-in; assuming the opt-in tier
// would be the more surprising guess of the two.
func CacheWriteWeight(ttlSeconds int64) float64 {
	if ttlSeconds == CacheTTL1hSeconds {
		return WCacheWrite1h
	}
	return WCacheWrite5m
}

// DefaultMultiplier is applied to any model absent from the table.
//
// It is deliberately Opus-tier (1.0) rather than 0: an unrecognized model
// is far more likely to be a newly released one not yet priced than a free
// one, and under-counting usage is the more damaging error for a tool whose
// whole job is predicting limit exhaustion. Surfaces that would rather drop
// an unpriced model than assume a price check Known and exclude it
// themselves; this is the floor for the ones that must weight every turn.
const DefaultMultiplier = 1.0

// Multiplier returns the model's cost weight relative to the baseline,
// read from the active price table (prices.go). Unknown models fall back to
// DefaultMultiplier.
func Multiplier(model string) float64 {
	w, _ := Current().Multiplier(model)
	return w
}

// Known reports whether the model is priced in the active table. Callers
// that surface confidence to the user (the per-model page) use this to mark
// rows whose weight is a fallback guess rather than a known ratio.
func Known(model string) bool {
	return Current().Known(model)
}

// Models lists every priced model in the active table, sorted. Used by the
// per-model page and by tests that assert Go and SQL weighting agree.
func Models() []string {
	return Current().ModelNames()
}

// CW returns cost-weighted tokens for one turn.
func CW(model string, in, out, cacheRead, cacheWrite5m, cacheWrite1h int64) float64 {
	typed := float64(in)*WInput +
		float64(out)*WOutput +
		float64(cacheRead)*WCacheRead +
		float64(cacheWrite5m)*WCacheWrite5m +
		float64(cacheWrite1h)*WCacheWrite1h
	return typed * Multiplier(model)
}

// qualify prefixes a column with a table alias, if one was given.
// Queries that join `turns` against a table sharing its column names
// (calibration_points has output_tokens too) must pass an alias or SQLite
// rejects the expression as ambiguous.
func qualify(alias, col string) string {
	if alias == "" {
		return col
	}
	return alias + "." + col
}

// TypeExprFor weights a turn's columns by token type only, with no model
// factor, qualified by the given table alias ("" for none).
//
// This is the shape the weight estimator needs: it must observe tokens
// *without* the model multiplier applied, or it would just re-derive its
// own input. See learn.go.
//
// Built from the W* constants above (via %g, which renders e.g. 1.0 as
// "1" and 1.25 as "1.25") rather than repeating the weights as string
// literals, so the package doc's promise that Go and SQL cannot drift is
// actually true.
func TypeExprFor(alias string) string {
	q := func(c string) string { return qualify(alias, c) }
	return fmt.Sprintf("(%s * %g + %s * %g + %s * %g + %s * %g + %s * %g)",
		q("input_tokens"), WInput,
		q("output_tokens"), WOutput,
		q("cache_read"), WCacheRead,
		q("cache_create_5m"), WCacheWrite5m,
		q("cache_create_1h"), WCacheWrite1h,
	)
}

// TypeExpr is TypeExprFor with no alias — the common case.
var TypeExpr = TypeExprFor("")

// ModelCaseExprFor is the SQL CASE mapping a `model` column to its
// multiplier, qualified by the given table alias ("" for none). Exported
// so callers with their own token-type shape (the cold-prefix estimate,
// which re-prices a whole prefix as cache creation) can apply the model
// factor without duplicating the table.
func ModelCaseExprFor(alias string) string {
	t := Current()
	var b strings.Builder
	b.WriteString("(CASE " + qualify(alias, "model"))
	for _, m := range t.ModelNames() {
		w, _ := t.Multiplier(m)
		fmt.Fprintf(&b, " WHEN '%s' THEN %g", m, w)
	}
	fmt.Fprintf(&b, " ELSE %g END)", DefaultMultiplier)
	return b.String()
}

// ModelCaseExpr is ModelCaseExprFor with no alias.
func ModelCaseExpr() string { return ModelCaseExprFor("") }

// SQLExprFor is the full cost-weighted expression — token type times model
// multiplier — qualified by the given table alias ("" for none). It is
// generated from the same table Multiplier reads, so the SQL and Go paths
// cannot disagree.
//
// The expression assumes the queried table exposes a `model` column and
// the five token columns.
func SQLExprFor(alias string) string {
	return "(" + TypeExprFor(alias) + " * " + ModelCaseExprFor(alias) + ")"
}

// SQLExpr is SQLExprFor with no alias — the common case, for queries
// selecting from `turns` alone.
func SQLExpr() string { return SQLExprFor("") }
