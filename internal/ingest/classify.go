package ingest

// Classification labels. Kept in sync with the per-session view of the POC.
const (
	ClassNormal      = "normal"
	ClassIdleMiss    = "idle_miss"
	ClassRotation    = "rotation"
	ClassRestructure = "restructure"
)

// Classify a single assistant turn.
//
//	normal       — typical turn (cache_read dominates, small cw delta)
//	idle_miss    — gap > 5 min and significant cache_creation: the cache
//	               expired during idle time and the prefix had to be re-cached
//	rotation     — cache_creation > 10k and cache_read < 1k: Claude Code
//	               rotated cache breakpoints (4-breakpoint API limit)
//	restructure  — both cr and cw significant in the same turn (e.g. image
//	               upload shifted the prefix). Distinct from rotation.
//
// Thresholds are tuned empirically and match the POC. We may revisit once
// we have community-insights data across many accounts.
func Classify(cw5m, cw1h, cr int, gapS float64) string {
	cw := cw5m + cw1h
	total := cw + cr
	if total < 1000 {
		return ClassNormal
	}
	if gapS > 300 && cw > 3000 {
		return ClassIdleMiss
	}
	if cw > 10000 && cr < 1000 {
		return ClassRotation
	}
	if cw > 10000 && float64(cw)/float64(total) > 0.20 {
		return ClassRestructure
	}
	return ClassNormal
}
