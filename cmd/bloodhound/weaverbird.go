package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	wb "github.com/PeterSR/claude-code-weaverbird/provider"

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
	"github.com/PeterSR/claude-code-bloodhound/internal/nowstate"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

// weaverbirdSpec declares bloodhound's two default quota gauges plus two
// opt-in detail widgets to a weaverbird host. All four are kind: text
// (weaverbird SPEC.md section 3.2). kind: meter is deliberately ruled out
// for .5h/.week: bloodhound wants control over the exact string, so it
// keeps composing its own and is not a candidate for meter/series/
// timestamp regardless of what the shape of any one widget might suggest.
//
// Two default widgets, not one: 5h and week are independently meaningful
// facts, a user tracks each on its own, so weaverbird must be free to
// order, color, cache, and drop them separately under width pressure. Per
// weaverbird's widget-split rule (SPEC.md section 3.3 and 6) that makes
// them two widgets, not a single widget carrying two colored spans.
//
// bloodhound.burn and bloodhound.poll are opt-in (Default: wb.OptIn()):
// useful detail a user can add to their layout, but noise in the common
// case where 5h/week already say enough. They stay out of the implicit
// default group and the no-layout view, reachable only by widget id or via
// the "bloodhound.detail" group declared below. Keeping the two default
// widgets first in this slice matters: the implicit default group is equal
// to Widgets in this order, so the opt-in pair must not lead it.
var weaverbirdSpec = wb.Spec{
	V:        1,
	Provider: "bloodhound",
	Icon:     "🩸",
	Widgets: []wb.Widget{
		{
			ID:       "bloodhound.5h",
			Title:    "5 hour quota",
			Kind:     wb.KindText,
			Priority: 10,
			Cache:    &wb.Cache{TTLSec: 10},
		},
		{
			ID:       "bloodhound.week",
			Title:    "Weekly quota",
			Kind:     wb.KindText,
			Priority: 20,
			Cache:    &wb.Cache{TTLSec: 10},
		},
		{
			ID:       "bloodhound.burn",
			Title:    "5h burn rate",
			Kind:     wb.KindText,
			Priority: 15,
			Row:      1,
			Default:  wb.OptIn(),
			Cache:    &wb.Cache{TTLSec: 10},
		},
		{
			ID:       "bloodhound.poll",
			Title:    "Poll freshness",
			Kind:     wb.KindText,
			Priority: 5,
			Row:      1,
			Default:  wb.OptIn(),
			Cache:    &wb.Cache{TTLSec: 10},
		},
	},
	Groups: []wb.Group{
		{
			ID:      "bloodhound.detail",
			Title:   "Quota detail",
			Widgets: []string{"bloodhound.5h", "bloodhound.week", "bloodhound.burn", "bloodhound.poll"},
		},
	},
}

// weaverbirdCmd is the additive integration point: it does not touch
// statusCmd or internal/statusline, it only adds a new subcommand that
// speaks weaverbird's provider contract over the same data nowstate.Compute
// already serves to `bloodhound now --json` and GET /api/now.
var weaverbirdCmd = &cobra.Command{
	Use:   "weaverbird",
	Short: "Emit a weaverbird provider spec or value records",
	Long: `weaverbird is a statusline multiplexer for Claude Code. This
subcommand implements its provider contract so bloodhound can run as one
of several widgets in a shared statusline instead of owning the whole
line.

  bloodhound weaverbird spec           prints the provider's capability document
  bloodhound weaverbird value [ids...] prints current widget values as NDJSON

"value" reads the Claude Code session JSON from stdin (see weaverbird's
SPEC.md); bloodhound does not currently need any session field, but reads
it anyway so a caller that always pipes session JSON never blocks.`,
	DisableFlagParsing: true, // pass "spec"/"value" and any widget ids through untouched
	RunE: func(cmd *cobra.Command, args []string) error {
		return wb.Dispatch(args, cmd.InOrStdin(), cmd.OutOrStdout(), weaverbirdSpec, weaverbirdValue)
	},
}

func init() {
	rootCmd.AddCommand(weaverbirdCmd)
}

// weaverbirdValue reuses nowstate.Compute, the single implementation
// `bloodhound now --json` and GET /api/now already share, so this widget
// can never disagree with what a user sees running bloodhound directly. It
// never re-parses the colored `bloodhound status` string.
//
// No store, no observation yet, or a computation error all degrade to "no
// widgets this render" rather than an error: a quiet bloodhound section is
// better than a weaverbird render that fails because one provider had
// nothing to say yet.
func weaverbirdValue(_ wb.Session, _ []string) ([]wb.Value, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	s, err := store.Open(ctx)
	if err != nil {
		return nil, nil
	}
	defer s.Close()

	out, err := nowstate.Compute(ctx, s, time.Now())
	if err != nil || out == nil {
		return nil, nil
	}

	var vals []wb.Value
	if out.Session != nil {
		vals = append(vals, bucketValue("bloodhound.5h", "5h", out.Session))
	}
	if out.Week != nil {
		vals = append(vals, bucketValue("bloodhound.week", "wk", out.Week))
	}
	if v := burnValue(out.Session); v != nil {
		vals = append(vals, *v)
	}
	if v := pollValue(out.LastPoll, out.StaleAfterS); v != nil {
		vals = append(vals, *v)
	}
	return vals, nil
}

// bucketValue turns one NowWindow into a value record. weaverbird has no
// host-side threshold map (SPEC.md section 4.1: class resolution is one
// rule, the class on the record), so a percentage never derives a color on
// its own. bucketValue computes its own class via
// classForPercentage, then overrides it when the percentage alone would
// not capture the urgency the bucket actually carries: saturated (pinned
// at the cap, extra-usage billing has kicked in) or a burn-rate projection
// that would hit 100% before the natural reset, while the percentage
// itself is still below the danger threshold.
//
// The parenthetical qualifier ("(saturated)", "(limit 33m)") stays inside
// this one widget's text rather than becoming a widget of its own: it is
// not a fact that stands alone, it only explains this gauge's own number,
// so it takes the same single class as the rest of the clause. No second
// color needed inside one widget.
func bucketValue(id, label string, w *routes.NowWindow) wb.Value {
	text := fmt.Sprintf("%s %d%%", label, w.Pct)
	switch {
	case w.Saturated:
		text += " (saturated)"
	case w.LimitOK:
		text += " (limit " + fmtResetDur(w.LimitETAMS) + ")"
	case w.TimeToResetMS > 0:
		text += " (" + fmtResetDur(w.TimeToResetMS) + ")"
	}

	var explicit string
	if (w.Saturated || w.LimitOK) && w.Pct < 90 {
		explicit = wb.ClassDanger
	}

	return wb.Value{
		ID:        id,
		FullText:  text,
		ShortText: fmt.Sprintf("%d%%", w.Pct),
		Class:     classForPercentage(explicit, w.Pct),
	}
}

// classForPercentage is bloodhound's own severity rule, computed here
// because the provider is the only party that can: weaverbird takes the
// class off the record and never derives one (weaverbird SPEC.md section
// 4.1). Danger at or above 90,
// warn at or above 70, else ok. explicit, when non-empty, is an
// already-decided override (bucketValue's saturated/limit-projection
// case) and always wins outright — matching ResolveClass's "explicit class
// on the record wins" rule, so intent stated directly by the provider
// still beats the threshold calculation.
func classForPercentage(explicit string, pct int) string {
	if explicit != "" {
		return explicit
	}
	switch {
	case pct >= 90:
		return wb.ClassDanger
	case pct >= 70:
		return wb.ClassWarn
	default:
		return wb.ClassOK
	}
}

// burnValue is the opt-in "bloodhound.burn" widget, built from the same
// 5-hour NowWindow bucketValue already renders (out.Session): no separate
// probe, no store read of its own. It picks whichever of the window's two
// burn-rate facts is more actionable and says only that one, rather than
// cramming both into one line:
//
//   - LimitOK: the projection already says this window would hit 100%
//     before its natural reset. That ETA is the whole story, so the rate
//     that produced it is left out.
//   - BurnOK (LimitOK false): no imminent breach, but a slope was still
//     measurable — report the current rate for a user tracking pace.
//   - Neither: fewer than two recent points, or the two points were not
//     far enough apart to derive a slope (see nowstate.fillBurn /
//     slopeOver). Nothing measurable to say, so the widget is silent this
//     render — same "omit rather than fake a zero" rule bucketValue and
//     weaverbirdValue's no-data path already follow.
//
// Returns nil (not a zero Value) for the silent case so the caller can
// `if v := burnValue(...); v != nil` without a second zero-value check.
func burnValue(w *routes.NowWindow) *wb.Value {
	if w == nil {
		return nil
	}
	switch {
	case w.LimitOK:
		eta := fmtResetDur(w.LimitETAMS)
		class := wb.ClassWarn
		// Under half an hour out, a passive "warn" undersells it — the
		// window is about to actually saturate, not just trending that
		// way. Same "very near" cutoff style as internal/nowstate's own
		// judgment calls (see slopeOver's dtH/slope thresholds).
		if w.LimitETAMS <= 30*60*1000 {
			class = wb.ClassDanger
		}
		return &wb.Value{
			ID:        "bloodhound.burn",
			FullText:  fmt.Sprintf("burn 5h -> 100%% %s", eta),
			ShortText: eta,
			Class:     class,
		}
	case w.BurnOK:
		rate := fmtBurnRate(w.BurnPctPerHour)
		return &wb.Value{
			ID:        "bloodhound.burn",
			FullText:  "burn 5h " + rate,
			ShortText: rate,
			Class:     wb.ClassNeutral,
		}
	default:
		return nil
	}
}

// fmtBurnRate renders a %/hour slope compactly: whole numbers print bare
// ("12%/h"), anything else keeps one decimal ("2.3%/h"). BurnPctPerHour
// already arrives rounded to 2 decimals (nowstate.round2 via fillBurn); this
// only controls display width, it does not re-round the underlying value.
func fmtBurnRate(pctPerHour float64) string {
	if pctPerHour == float64(int64(pctPerHour)) {
		return fmt.Sprintf("%d%%/h", int64(pctPerHour))
	}
	return fmt.Sprintf("%.1f%%/h", pctPerHour)
}

// pollValue is the opt-in "bloodhound.poll" widget: data freshness read
// straight off NowResponse.LastPoll and NowResponse.StaleAfterS, both
// already computed by nowstate.Compute for `bloodhound now --json` and GET
// /api/now, so this adds no probe and no store read of its own.
//
// p is nil only when Compute found no /usage observation at all yet (see
// nowstate.Compute's doc comment); that is the one silent case, matching
// weaverbirdValue's existing "no data yet" behavior for the two default
// widgets. Once there has been at least one observation, this widget always
// has something to say — even a failed extraction is worth surfacing,
// which is the point of a freshness widget.
//
// NowPoll has no separate "OK" field; ParseOK is the one flag Compute
// carries both on NowResponse.OK and NowPoll.ParseOK (Compute sets
// out.OK = obs.ParseOK, the same source value), so ParseOK alone covers
// "did not parse or is not OK".
func pollValue(p *routes.NowPoll, staleAfterS int) *wb.Value {
	if p == nil {
		return nil
	}
	if !p.ParseOK {
		return &wb.Value{
			ID:        "bloodhound.poll",
			FullText:  "extraction failed",
			ShortText: "failed",
			Class:     wb.ClassDanger,
		}
	}

	age := fmtResetDur(p.AgeS * 1000)
	if staleAfterS > 0 && p.AgeS > int64(staleAfterS) {
		return &wb.Value{
			ID:        "bloodhound.poll",
			FullText:  "poll stale " + age,
			ShortText: age,
			Class:     wb.ClassStale,
		}
	}
	return &wb.Value{
		ID:        "bloodhound.poll",
		FullText:  "poll " + age,
		ShortText: age,
		Class:     wb.ClassNeutral,
	}
}

// fmtResetDur renders a millisecond duration the same coarse-to-fine way
// the existing statusline does ("33m", "3h12m", "5d3h"). Re-declared here
// rather than exported from internal/statusline: it is a one-line
// formatting helper, not part of the computation that package exists to
// single-source, the same tradeoff internal/nowstate's own round2 already
// makes.
func fmtResetDur(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	}
	day := int(d.Hours()) / 24
	rh := int(d.Hours()) - day*24
	if rh == 0 {
		return fmt.Sprintf("%dd", day)
	}
	return fmt.Sprintf("%dd%dh", day, rh)
}
