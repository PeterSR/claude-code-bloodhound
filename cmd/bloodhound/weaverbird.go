package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	wb "github.com/PeterSR/claude-code-weaverbird/provider"

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
	"github.com/PeterSR/claude-code-bloodhound/internal/nowstate"
	"github.com/PeterSR/claude-code-bloodhound/internal/sessioninsight"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
	"github.com/PeterSR/claude-code-bloodhound/internal/whenfmt"
)

// The three single-cell markers a bucket's parenthetical uses to label
// what each duration inside it means. They exist so the parenthetical can
// carry the projected-100% ETA and the natural reset at the same time
// without either being ambiguous: before this, a limit projection
// suppressed the reset countdown entirely, because "(33m)" alone could
// not say which of the two it was.
//
// These were picked by rendering a probe sheet of candidates in a real
// terminal, not by reading width tables, because the two ways a glyph
// goes wrong here look nothing alike on paper and are easy to confuse
// with each other:
//
//   - Double width. U+26A1, U+26D4 and even U+2588 FULL BLOCK take two
//     cells in common terminal fonts, silently costing a column the width
//     cascade already budgeted. The tell is that everything after the
//     glyph shifts right by one.
//   - Glyph overhang. The terminal allots one cell, but the font draws
//     wider than it and bleeds over the neighbouring cell. Nothing
//     shifts; the character after it is simply painted on. This is what
//     U+26A0 does, and limitPad below is the fix.
//
// U+27F3 was the first glyphReset and lost to U+21BB for a third reason
// again: the supplemental arrows blocks have far thinner monospace
// coverage than the base Arrows block, so it landed as a tofu-adjacent
// shape. Prefer Arrows (U+2190..21FF) and Geometric Shapes
// (U+25A0..25FF) when picking a replacement.
const (
	glyphLimit     = "⚠" // U+26A0: burn rate projects this window hits 100% before its reset
	glyphSaturated = "⊘" // U+2298: pinned at the cap, pct has stopped moving
	glyphReset     = "↻" // U+21BB: time until this window's natural reset
	glyphStale     = "◷" // U+25F7: how long ago this carried-over reading was actually taken
	glyphLowPri    = "↓" // U+2193: at the cap but still working, at the lower request priority

	// glyphLowPri replaces glyphSaturated rather than joining it, and the
	// substitution is the whole point of the mark. ⊘ reads as a stop,
	// which is exactly right when requests are being refused and exactly
	// wrong when they are still going through slowly; a user who reads a
	// stop where there is none stops working for no reason, which is the
	// more expensive of the two mistakes.
	//
	// U+2193 obeys the block rule this file's header sets out: base Arrows,
	// single cell everywhere, no overhang, and it needs no limitPad. It also
	// carries the right meaning without a legend, which the alternatives in
	// Geometric Shapes did not.

	// glyphStale is the one circular shape next to glyphReset's circular
	// arrow, which is a real cost: at terminal sizes ◷ and ↻ are more alike
	// than either is to ⚠ or ⊘. It wins anyway because it sits in Geometric
	// Shapes, the block this file's header calls out as reliably covered by
	// monospace fonts, and the hourglasses that read better semantically do
	// not: U+29D6/U+29D7 are in the sparse Misc Math Symbols-B block and
	// risk the same tofu that cost U+27F3 its place, while U+231B/U+23F3
	// are East Asian Wide and would silently eat two cells from a width
	// cascade budgeted for one. A pair of similar circles is a legibility
	// cost; tofu and a width overrun are correctness ones.

	// limitPad sits between glyphLimit and its duration and is load
	// bearing, not spacing taste. U+26A0's glyph overhangs its cell to
	// the right in many terminal fonts and would otherwise be drawn over
	// the first digit — "⚠33m" renders as an unreadable smudge where the
	// triangle and the 3 share a cell. A space gives the overhang
	// somewhere harmless to land, which is what lets the mark keep the
	// one code point that unambiguously reads as a warning.
	//
	// glyphSaturated and glyphReset need no equivalent: both are drawn
	// well within their cell, so padding them would cost a column and buy
	// nothing.
	limitPad = " "
)

// weaverbirdSpec declares bloodhound's three default widgets plus three
// opt-in detail widgets to a weaverbird host. All six are kind: text
// (weaverbird SPEC.md section 3.2). kind: meter is deliberately ruled out
// for .5h/.week: bloodhound wants control over the exact string, so it
// keeps composing its own and is not a candidate for meter/series/
// timestamp regardless of what the shape of any one widget might suggest.
//
// Two default gauges, not one: 5h and week are independently meaningful
// facts, a user tracks each on its own, so weaverbird must be free to
// order, color, cache, and drop them separately under width pressure. Per
// weaverbird's widget-split rule (SPEC.md section 3.3 and 6) that makes
// them two widgets, not a single widget carrying two colored spans.
//
// The two bloodhound.resume.* widgets are one cost priced against two
// pools, and they are a pair for exactly the reason the two gauges above
// are: a cold resume that is 13% of the 5-hour window is around 1.4% of
// the weekly one, which is danger and info respectively. A value record
// carries a single class, so folding both readings into one widget would
// force one of the two colors to be wrong. Two numbers, two severities,
// two widgets.
//
// bloodhound.resume.5h is the third default and the one always-on widget
// here that is silent most of the time by construction: it emits a record
// only while the current session's prompt cache has actually gone cold
// (see resumeValues). A widget that suppresses itself does not need to be
// opt-in — the cost of leaving it on is zero in every render where it has
// nothing to say — so it earns a place in the default group that
// always-on detail like burn and poll does not.
//
// bloodhound.resume.week, bloodhound.burn and bloodhound.poll are opt-in
// (Default: wb.OptIn()): useful detail a user can add to their layout, but
// noise in the common case where 5h/week already say enough. They stay out
// of the implicit default group and the no-layout view, reachable only by
// widget id or via the "bloodhound.detail" group declared below. Keeping
// the three default widgets first in this slice matters: the implicit
// default group is equal to Widgets in this order, so the opt-in widgets
// must not lead it.
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
			// Highest priority of the six, which reads odd for a
			// widget that is usually absent and is exactly why it
			// wins: priority only ever arbitrates width pressure, and
			// the renders where this widget exists at all are the
			// renders where it is the most actionable thing on the
			// line. The two gauges it outranks report a slow-moving
			// number the user can re-read on the next render; a cold
			// cache is a cost the very next turn pays, and dropping
			// it under width pressure drops it for good.
			ID:       "bloodhound.resume.5h",
			Title:    "Cold-cache resume cost (5h)",
			Kind:     wb.KindText,
			Priority: 25,
			DataDeps: []string{"session"},
			Cache:    &wb.Cache{TTLSec: 10},
		},
		{
			// The same cost priced against the weekly pool, and
			// deliberately NOT ranked like its 5h twin. The weekly
			// pool is roughly an order of magnitude larger, so the
			// same cold resume is a single-digit fraction of it at
			// worst — real, worth showing, but never the thing you
			// drop a gauge to keep. Priority 8 puts it first out under
			// width pressure among the top-row widgets, which is the
			// same "how actionable is this reading" test that put its
			// twin at 25, applied honestly to a smaller number.
			//
			// Opt-in for the same reason: it is detail, in a way the
			// 5h reading is not.
			ID:       "bloodhound.resume.week",
			Title:    "Cold-cache resume cost (weekly)",
			Kind:     wb.KindText,
			Priority: 8,
			DataDeps: []string{"session"},
			Default:  wb.OptIn(),
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
			ID:    "bloodhound.detail",
			Title: "Quota detail",
			Widgets: []string{
				"bloodhound.5h", "bloodhound.week",
				"bloodhound.resume.5h", "bloodhound.resume.week",
				"bloodhound.burn", "bloodhound.poll",
			},
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
//
// The pool-state widgets and bloodhound.resume are computed independently
// of each other, and a failure in one does not silence the other. They
// read disjoint data — /usage observations versus the local turns table —
// so a machine that has ingested transcripts but has not landed a
// successful /usage poll yet can still price a cold resume, and vice
// versa.
func weaverbirdValue(sess wb.Session, requested []string) ([]wb.Value, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	now := time.Now()

	s, err := store.Open(ctx)
	if err != nil {
		return nil, nil
	}
	defer s.Close()

	var vals []wb.Value
	// The meter of the account this session spends, falling back to the
	// primary when there is no session or it has not been ingested yet.
	acct, _ := s.PrimaryAccountID(ctx)
	if sess.SessionID != "" {
		acct, _ = s.AccountForSession(ctx, sess.SessionID)
	}
	if out, err := nowstate.Compute(ctx, s, acct, now); err == nil && out != nil {
		if out.Session != nil {
			vals = append(vals, bucketValue("bloodhound.5h", "5h", out.Session, now))
		}
		if out.Week != nil {
			vals = append(vals, bucketValue("bloodhound.week", "wk", out.Week, now))
		}
		if v := burnValue(out.Session); v != nil {
			vals = append(vals, *v)
		}
		if v := pollValue(out.LastPoll, out.StaleAfterS); v != nil {
			vals = append(vals, *v)
		}
	}

	// The only widgets gated on `requested`. The four above are all read
	// off a single nowstate.Compute the provider runs regardless, so
	// filtering them here would save nothing and weaverbird drops what it
	// did not ask for anyway (SPEC.md section 2.2: the ids are a hint).
	// These cost extra queries against the turns table, which is worth not
	// paying on every render when the user's layout asked for neither.
	// One gate for both, since they share the query that makes them
	// expensive; weaverbird filters out whichever of the pair the layout
	// did not want.
	if wants(requested, "bloodhound.resume.5h") || wants(requested, "bloodhound.resume.week") {
		vals = append(vals, resumeValues(ctx, s, sess.SessionID, now)...)
	}
	return vals, nil
}

// wants reports whether weaverbird asked for id this render. An empty
// requested means "everything you have": weaverbird appends the ids it
// wants as trailing arguments, so no ids at all is a caller running the
// subcommand by hand rather than a host asking for nothing.
func wants(requested []string, id string) bool {
	if len(requested) == 0 {
		return true
	}
	for _, r := range requested {
		if r == id {
			return true
		}
	}
	return false
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
// The parenthetical qualifier stays inside this one widget's text rather
// than becoming a widget of its own: it is not a fact that stands alone,
// it only explains this gauge's own number, so it takes the same single
// class as the rest of the clause. No second color needed inside one
// widget.
//
// It carries up to two marks, each a single-cell glyph followed by its
// duration, and the pressure mark never suppresses the reset countdown:
//
//	5h 72% (↻3h12m)         nothing unusual, resets in 3h12m
//	5h 72% (⚠ 33m ↻3h12m)   projects 100% in 33m, well before that reset
//	5h 99% (⊘ ↻3h12m)       already pinned at the cap, resets in 3h12m
//	5h 100% (↓ ↻1h04m)      at the cap and still working, at low priority
//	wk 39% (↻fri 15:30)     a day or more out, so named rather than counted
//
// Only the reset mark can go absolute (see whenfmt.Deadline). The limit mark
// stays a countdown at any distance on its own merits: it is a projection
// off a measured burn slope with real error bars, and naming a weekday and
// a clock time for it would dress that up in a precision it does not have.
//
// The earlier form spent the whole parenthetical on whichever mark won
// and dropped the other, so precisely when a window was under pressure —
// the moment "how long until this clears?" is the actual question — the
// answer to it disappeared. Two marks cost two glyphs plus a space over
// the old "(limit 33m)", which is cheap enough that width pressure is
// weaverbird's problem to solve via ShortText, not a reason to withhold
// the fact. Saturated carries no duration of its own because there isn't
// one: the pct has stopped moving, so there is no slope left to project
// an ETA from.
func bucketValue(id, label string, w *routes.NowWindow, now time.Time) wb.Value {
	text := fmt.Sprintf("%s %d%%", label, w.Pct)

	var marks []string
	switch {
	case w.CapState == routes.CapLowPriority:
		// Outranks plain saturation, and is checked before it, because
		// the two disagree: the meter says pinned, the transcripts say
		// requests are still going through. The transcripts are the ones
		// that saw an actual request.
		marks = append(marks, glyphLowPri)
	case w.Saturated:
		// Saturated outranks the limit projection rather than joining
		// it: "will hit the cap" is not worth saying next to "is at the
		// cap".
		marks = append(marks, glyphSaturated)
	case w.LimitOK:
		marks = append(marks, glyphLimit+limitPad+whenfmt.Dur(w.LimitETAMS))
	}
	if w.TimeToResetMS > 0 {
		marks = append(marks, glyphReset+fmtReset(w, now))
	}
	// The stale mark goes last so the marks stay in "what is happening to
	// this window" order, with "and this reading is old" as the qualifier
	// on all of it rather than something interleaved among the facts.
	if w.Stale {
		marks = append(marks, glyphStale+staleAge(w.StaleTSISO, now))
	}
	if len(marks) > 0 {
		text += " (" + strings.Join(marks, " ") + ")"
	}

	// Low priority is the one case where the class is talked DOWN rather
	// than up, and it has to override the percentage outright: 100% resolves
	// to danger on its own, and danger is what a user reads as "stop". They
	// are not stopped. Warn is the honest reading — work continues, more
	// slowly, and it is coming out of the weekly allowance now.
	var explicit string
	switch {
	case w.CapState == routes.CapLowPriority:
		explicit = wb.ClassWarn
	case (w.Saturated || w.LimitOK) && w.Pct < 90:
		explicit = wb.ClassDanger
	}

	return wb.Value{
		ID:        id,
		FullText:  text,
		ShortText: fmt.Sprintf("%d%%", w.Pct),
		Class:     classForStaleness(classForPercentage(explicit, w.Pct), w.Stale),
	}
}

// classForStaleness downgrades a carried-over reading to weaverbird's
// stale class, except when the reading is already danger.
//
// Both classes are true of a stale 92%, and only one can be rendered. The
// tie goes to danger because the two errors are not symmetric: colouring a
// stale reading as current overstates freshness by at most one poll
// interval, while colouring an at-the-cap reading as merely stale
// understates a number that is about to stop the user's work. The glyph in
// the text says it is old either way, so nothing is hidden by keeping the
// colour on the severity.
func classForStaleness(class string, stale bool) string {
	if !stale || class == wb.ClassDanger {
		return class
	}
	return wb.ClassStale
}

// staleAge renders how old a carried-over reading is. Falls back to the
// bare glyph when the timestamp is unparseable: that the reading is stale
// is the load-bearing half, and it is already known from w.Stale without
// trusting the string.
func staleAge(tsISO string, now time.Time) string {
	t, err := time.Parse(time.RFC3339, tsISO)
	if err != nil {
		return ""
	}
	age := now.Sub(t)
	if age < 0 {
		return ""
	}
	return whenfmt.Dur(age.Milliseconds())
}

// classForPercentage is bloodhound's own severity rule, computed here
// because the provider is the only party that can: weaverbird takes the
// class off the record and never derives one (weaverbird SPEC.md section
// 4.1). Danger at or above 90,
// warn at or above 70, else ok. explicit, when non-empty, is an
// already-decided override and always wins outright — matching
// ResolveClass's "explicit class on the record wins" rule, so intent
// stated directly by the provider still beats the threshold calculation.
//
// bucketValue sets it in both directions. Upward for saturation and for a
// limit projection under a percentage that has not caught up yet; downward
// for a window at the cap that is nonetheless still serving requests at low
// priority, where the percentage's own answer of danger would tell a
// working user to stop.
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
		eta := whenfmt.Dur(w.LimitETAMS)
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

	age := whenfmt.Dur(p.AgeS * 1000)
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

// Severity thresholds for a cold-resume cost, as a percentage of the
// 5-hour window it is charged against. Under resumeWarnPct the cost is
// noise against a 100-point pool — you would need thirty-odd cold resumes
// to spend the window. Past resumeDangerPct one resume costs a tenth of
// it, which is the point where starting fresh or trimming the session is
// the cheaper move, so the widget says so in the same color a 90%+ gauge
// would.
const (
	resumeWarnPct   = 3.0
	resumeDangerPct = 10.0
)

// resumeValues builds the "bloodhound.resume.*" widgets: what it will
// cost to carry the current session forward now that its prompt cache has
// gone cold. Silent in every other case, which is most of them — a warm
// session has nothing to pay and nothing to say.
//
// The underlying figure is sessioninsight.ColdResumeCostCWTokens, the same
// one the Now page and the legacy `bloodhound status` line already show,
// so these can never disagree with them about the price. It is a floor,
// not a forecast: the conversation prefix is fixed and has to be re-cached
// at the cache_create rate, but the next prompt and response are on top of
// that and unknowable from here. sessioninsight only populates it once the
// session's age has actually crossed the TTL its last turn cached at (5m
// or 1h), so "cold" here is measured, not assumed.
//
// That one cost is then divided by each pool's own tokens-per-1% median to
// get two percentages. Only the conversion differs between the widgets —
// there is a single cold-resume cost, and the two records are two ways of
// reading it, which is why this is one function and one query rather than
// two independent widget builders.
//
// sessionUUID comes from the Claude Code session JSON weaverbird pipes in
// on stdin. Unlike internal/statusline, an unknown or absent id does not
// fall back to the most-recent active session. The two surfaces differ
// here for a reason: a fallback answers "is anything cold?", and these
// widgets answer "is the session you are typing in right now cold?" —
// quoting another session's resume price next to the current session's
// quota would be a wrong answer, not a degraded one. A session Claude Code
// has opened but bloodhound has not ingested yet is simply silent until
// the ingester catches up.
func resumeValues(ctx context.Context, s *store.Store, sessionUUID string, now time.Time) []wb.Value {
	if sessionUUID == "" {
		return nil
	}
	ref, err := sessioninsight.BySessionUUID(ctx, s.DB, sessionUUID)
	if err != nil || ref == nil {
		return nil
	}

	acct, _ := s.AccountForSession(ctx, sessionUUID)
	sessionPerPct, _, _, hasSessionCal, _ := s.LatestCalibrationMedian(ctx, acct, "session", 10)
	ins := sessioninsight.ForSession(ctx, s.DB, *ref, now, sessionPerPct, hasSessionCal)
	if ins == nil || ins.ColdResumeCostCWTokens <= 0 {
		return nil
	}

	// ColdResumeCostPct is already the 5h conversion, done by ForSession
	// against the median passed in above. Reusing it rather than dividing
	// again here keeps this widget on exactly the arithmetic the Now page
	// uses; only the weekly pool needs a conversion of its own.
	out := []wb.Value{resumeRecord("bloodhound.resume.5h", "5h", ins.ColdResumeCostPct)}

	// The weekly widget is silent without a weekly calibration rather than
	// falling back to the unpriced form. Unpriced, it would say only "the
	// cache is cold", which the 5h record beside it already says — a
	// second widget repeating it is noise, where the first one saying it
	// is the whole point.
	if weekPerPct, _, _, ok, err := s.LatestCalibrationMedian(ctx, acct, "week", 10); err == nil && ok && weekPerPct > 0 {
		out = append(out, resumeRecord("bloodhound.resume.week", "wk",
			ins.ColdResumeCostCWTokens/weekPerPct))
	}
	return out
}

// resumeRecord renders one pool's view of the cold-resume cost. The pool
// label leads, matching the two gauges ("5h 72%", "wk 39%") so the bar
// reads consistently left to right, and avoiding the misparse that puts
// it last: "resume 5h" beside a line full of durations reads as "resume
// in 5 hours" rather than "against the 5h pool".
//
// pct <= 0 means there is no calibration to price this pool with. The
// record still fires, because knowing the cache is cold is actionable on
// its own, and it says exactly that instead of printing a raw
// cost-weighted token count, which is a number with no scale a user can
// act on.
func resumeRecord(id, pool string, pct float64) wb.Value {
	p := fmtCostPct(pct)
	if p == "" {
		return wb.Value{
			ID:        id,
			FullText:  pool + " resume cold",
			ShortText: pool + " cold",
			Class:     wb.ClassInfo,
		}
	}
	return wb.Value{
		ID:        id,
		FullText:  pool + " resume " + p,
		ShortText: pool + p,
		Class:     classForResumeCost(pct),
	}
}

// classForResumeCost grades a cold-resume cost against the thresholds
// above. Info rather than ok at the low end: an unavoidable cost the user
// is about to pay is never "ok" news, it is just small news, and ok is
// the class the two quota gauges use for a healthy reading.
func classForResumeCost(pct float64) string {
	switch {
	case pct >= resumeDangerPct:
		return wb.ClassDanger
	case pct >= resumeWarnPct:
		return wb.ClassWarn
	default:
		return wb.ClassInfo
	}
}

// fmtCostPct renders a cost as a signed, compact percentage: "+4%", or
// "+<1%" when it rounds away to nothing but is not zero. Returns "" when
// there is no figure to render (no calibration), which the caller reads
// as "say it is cold without pricing it".
//
// The leading "+" is load-bearing: without it "resume 4%" sits on a line
// beside "5h 72%" and reads as a fourth gauge at 4% rather than as 4
// points added to the one next to it. Mirrors internal/statusline's own
// renderPct and its "❄ cold +5%" output, re-declared here for the same
// reason internal/statusline's fmtDur stays where it is.
func fmtCostPct(v float64) string {
	if v <= 0 {
		return ""
	}
	if v < 1 {
		return "+<1%"
	}
	return fmt.Sprintf("+%d%%", int(v+0.5))
}

// fmtReset renders a window's reset mark, preferring the parsed absolute
// timestamp so whenfmt.Deadline can name a weekday, and falling back to the
// precomputed countdown when there is no parsable reset to name.
//
// The fallback is not dead code: nowstate.buildWindow leaves ResetTSISO
// empty whenever /usage did not carry a reset it could parse, and
// TimeToResetMS is then the only thing left. It cannot disagree with
// whenfmt.Deadline's own arithmetic, because now here is the same now Compute
// derived TimeToResetMS from.
func fmtReset(w *routes.NowWindow, now time.Time) string {
	if at, err := time.Parse(time.RFC3339, w.ResetTSISO); err == nil {
		return whenfmt.Deadline(at, now)
	}
	return whenfmt.Dur(w.TimeToResetMS)
}
