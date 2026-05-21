package usage

// Reapply re-runs the extractor against the cleaned panel already captured
// in res. Used by the daemon and CLI after the selfheal orchestrator has
// persisted a new extractor: we want the *same* poll to get the fresh
// values rather than waiting for the next cycle.
func Reapply(res Result, ext *Extractor) Result {
	res.Extracted = ext.Apply(res.RawFull)
	res.OK = len(res.Extracted.Missing) == 0
	res.ExtractorOrigin = string(OriginUser)
	res.SessionPct = nil
	res.WeekPct = nil
	res.SessionResetRaw = ""
	res.WeekResetRaw = ""
	res.SessionResetTZ = ""
	res.WeekResetTZ = ""
	if v, ok := res.Extracted.Values["session_pct"].(int); ok {
		res.SessionPct = &v
	}
	if v, ok := res.Extracted.Values["week_pct"].(int); ok {
		res.WeekPct = &v
	}
	if s, ok := res.Extracted.Values["session_reset"].(string); ok {
		res.SessionResetRaw = s
	}
	if s, ok := res.Extracted.Values["week_reset"].(string); ok {
		res.WeekResetRaw = s
	}
	if s, ok := res.Extracted.Values["session_reset_tz"].(string); ok {
		res.SessionResetTZ = s
	}
	if s, ok := res.Extracted.Values["week_reset_tz"].(string); ok {
		res.WeekResetTZ = s
	}
	return res
}
