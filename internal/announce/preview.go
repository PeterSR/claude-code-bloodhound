package announce

import (
	"time"

	ccsock "github.com/PeterSR/claude-code-socket-transport"

	"github.com/PeterSR/claude-code-bloodhound/internal/events"
	"github.com/PeterSR/claude-code-bloodhound/internal/projectconfig"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

// Preview renders what a project's config would actually produce.
//
// Through the real code path, not a description of it. Every field in the
// config is a template that runs once, hours from now, into a conversation
// nobody is watching, and the failure mode of the alternative (reading the
// JSON and imagining the sentence) is that the first render anyone sees is
// the one that went out wrong. Facts here are synthetic and obviously so, but
// the composition, the ordering and the wording are the same functions the
// daemon calls.
//
// Lives in announce rather than projectconfig because the built-in wording a
// template stands in for is here, and a preview that showed the template
// without what it replaces would be answering the smaller half of the
// question.
type Preview struct {
	// Pressure is a session hearing about its budget and a projected cap at
	// the same moment, which is the batch most of the config exists to shape.
	Pressure string
	// Resume is what arrives when the window reopens. Empty unless the
	// project asked bloodhound to carry the wakeup.
	Resume string
	// Armed reports whether that batch would have noted the session down.
	Armed bool
}

// RenderPreview builds one against a fixed synthetic situation: a directory
// two thirds through its allowance, a 5h window on pace to cap out forty
// minutes from now and reopen ninety minutes from now, and a session whose
// turns have started to outgrow its own average.
func RenderPreview(cfg projectconfig.Config, cwd string, now time.Time) Preview {
	iso := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	reset := iso(90 * time.Minute)

	sess := ccsock.Session{SessionID: "session-abc12345", CWD: cwd, Status: "busy"}
	evs := []events.Event{
		{
			ID:    1,
			Kind:  "budget.tight",
			Scope: events.Scope{Bucket: "session", Cwd: cwd},
			Detail: map[string]any{
				"reason":   "the 5h meter reads 16% and this directory stops at 20%",
				"reset_ts": reset,
			},
		},
		{
			ID:    2,
			Kind:  "limit_projection.projected",
			Scope: events.Scope{Bucket: "session"},
			Detail: map[string]any{
				"pct":            float64(42),
				"burn_pct_per_h": 18.5,
				"eta_ts":         iso(40 * time.Minute),
				"reset_ts":       reset,
			},
		},
		{
			ID:    3,
			Kind:  "recommendation.compact",
			Scope: events.Scope{Session: sess.SessionID, Project: "myapp"},
			Detail: map[string]any{
				"reason":           "recent turns are >2x the session average",
				"compact_cost_pct": 6.2,
				"cold_resume_pct":  18.4,
			},
		},
		{
			ID:    4,
			Kind:  "cache.expiring",
			Scope: events.Scope{Session: sess.SessionID, Project: "myapp"},
			Detail: map[string]any{
				"expires_in_s":    float64(540),
				"cold_resume_pct": 18.4,
			},
		},
	}

	msg := compose(evs, sess, now, cfg)
	out := Preview{Pressure: msg.Text, Armed: msg.Arm != nil}

	if cfg.Wakeup.Arms() {
		out.Resume = resumeText(store.SessionWakeup{
			SessionUUID: sess.SessionID,
			Cwd:         cwd,
			Bucket:      "session",
			Reason:      "Budget: the 5h meter reads 16% and this directory stops at 20%.",
			ArmedMS:     now.Add(-2 * time.Hour).UnixMilli(),
			DueMS:       now.UnixMilli(),
		}, sess, now, cfg)
	}
	return out
}
