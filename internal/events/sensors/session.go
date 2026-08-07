package sensors

import (
	"context"

	"github.com/PeterSR/claude-code-bloodhound/internal/events"
	"github.com/PeterSR/claude-code-bloodhound/internal/sessioninsight"
)

// sessionWindowS bounds which sessions get watched, and maxSessions bounds how
// many.
//
// Two hours rather than the 30 minute active threshold, because the longest
// prompt-cache TTL is an hour: a session idle 48 minutes on a 1h TTL is inside
// its expiry warning and a 30 minute window would have stopped looking long
// before. Past two hours every TTL has lapsed and the only fact left is the
// cold-resume cost, which does not change again.
const (
	sessionWindowS = 7200
	maxSessions    = 12
)

func init() {
	// The prompt cache is a cliff, not a slope: cross the TTL and the next
	// turn pays to rebuild the whole prefix. This is the one event in the set
	// with a hard deadline and a cheap action ("touch it now"), which is why
	// it wants the 60s reconcile tick rather than the 300s poll: a 5m TTL has
	// a deadline shorter than the poll interval.
	events.Register(events.Sensor{
		Name: "session/cache",
		Read: func(ctx context.Context, w events.World) ([]events.Reading, error) {
			refs, err := sessioninsight.RecentN(ctx, w.DB, w.Now, sessionWindowS, maxSessions)
			if err != nil {
				return nil, err
			}
			var out []events.Reading
			for _, ref := range refs {
				in := sessioninsight.ForSession(ctx, w.DB, ref, w.Now, w.TokensPerPctCW, w.HasCalibration)
				if in == nil {
					continue // nothing to say about a session we could not read
				}
				sc := events.Scope{Session: ref.UUID, Project: in.Project}

				r := events.Reading{Kind: "cache", Scope: sc}
				switch {
				case in.CacheTTLS == 0:
					// The TTL could not be inferred (a cache_read-only last
					// turn on a session with no cache_create history), so
					// neither expiry nor cold resume applies. Omit rather than
					// claim ignorance: there is genuinely nothing here.
					r.Kind = ""
				case in.ColdResumeCostCWTokens > 0:
					r.State = "expired"
					r.Detail = map[string]any{
						"ttl_s":                 in.CacheTTLS,
						"age_s":                 in.AgeS,
						"cold_resume_cw_tokens": in.ColdResumeCostCWTokens,
						"cold_resume_pct":       in.ColdResumeCostPct,
					}
				case in.CacheExpiresInS > 0:
					r.State = "expiring"
					r.Detail = map[string]any{
						"ttl_s":        in.CacheTTLS,
						"expires_in_s": in.CacheExpiresInS,
					}
				default:
					r.State = "warm"
					r.Detail = map[string]any{"ttl_s": in.CacheTTLS}
				}
				if r.Kind != "" {
					out = append(out, r)
				}

				// Whether recent turns have outgrown the session average.
				// Already computed per session; today it is only visible to
				// whoever happens to poll that session at the right moment.
				if in.Recommendation != "" {
					out = append(out, events.Reading{
						Kind:  "recommendation",
						Scope: sc,
						State: in.Recommendation,
						Detail: map[string]any{
							"reason":              in.RecommendationReason,
							"turn_count":          in.TurnCount,
							"compact_cost_pct":    in.CompactCostPct,
							"cold_resume_pct":     in.ColdResumeCostPct,
							"turns_since_compact": in.TurnsSinceCompact,
						},
					})
				}
			}
			return out, nil
		},
	})
}
