// Package attribute splits Anthropic's /usage limit meters across the
// Claude Code sessions that moved them.
//
// The meters are account-wide: /usage says "42%" of the 5-hour window and
// "18%" of the week, with no hint of which conversation spent it. But we
// also have every turn, its timestamp, its session, and its cost-weighted
// size. Between two readings inside the same limit window the meter moved a
// known number of points, and the turns in that span are what moved it, so
// splitting that movement pro-rata by cost-weighted tokens attributes the
// meter to sessions without ever having to trust a tokens-per-percent
// conversion. That split is capped at what the window's own reconciled rate
// says the local turns in the span could plausibly account for; whatever is
// left over is filed as unattributed rather than assumed local, because a
// tiny local turn sharing a span with a much bigger, unexplained move should
// not be charged for all of it.
//
// Where no pair of readings brackets a turn (before collection started,
// during a saturated stretch, the tail since the last poll) we fall back to
// the calibration median. The two kinds are accumulated separately and never
// merged, so a caller can always ask how much of a number was measured.
//
// Build is a pure function of its inputs, the same shape as capacity.go's
// weeklyCapacity and the burn-rate series, so the reconstruction is unit
// tested directly rather than through the database.
package attribute

import (
	"math"
	"sort"
	"time"
)

// Bucket values. Deliberately not calibration_points' "session" | "week":
// in this package "session" always means a Claude Code conversation, so the
// 5-hour limit window needed a name that could not be confused with one.
const (
	Bucket5h   = "5h"
	BucketWeek = "week"
)

// Unattributed is the sentinel session UUID for meter movement that local
// turns cannot plausibly account for: a second machine on the same account,
// or a JSONL we never read. "Cannot plausibly account for" is judged against
// the window's own reconciled rate (attributeMeasured's cap), not against
// there being zero local turns in the span, so one small local turn sharing
// a span with a much larger unexplained move still leaves most of that move
// here rather than charging it all to the small turn. Recorded as its own
// row rather than smeared over the real sessions, so the UI can show the gap
// honestly.
const Unattributed = ""

// Window-reconstruction thresholds, shared with the weekly-capacity view so
// the two reconstructions can never disagree about where a window begins.
//
//   - Ahead: how far in front of a reading its advertised reset may
//     plausibly sit. Further than that is a parse artifact, and we carry the
//     previous window forward instead of splitting on junk.
//   - MinGap: how far the advertised reset must jump forward to count as a
//     genuine rotation. A real reset jumps a whole period; a misparse shifts
//     the boundary by minutes to a couple of hours. Both gaps sit below the
//     real spacing and above the misparse range, and absorb the minute-level
//     jitter the extractor produces (23:00 vs 22:59).
const (
	WeekPeriod = 7 * 24 * time.Hour
	WeekAhead  = 8 * 24 * time.Hour
	WeekMinGap = 2 * 24 * time.Hour
	SessPeriod = 5 * time.Hour
	SessAhead  = 6 * time.Hour
	SessMinGap = 4 * time.Hour
)

// Obs is the slice of a /usage observation this reconstruction needs, for
// one bucket. The caller picks session_* or week_* columns off the row.
type Obs struct {
	TSUnixMS int64
	Pct      int
	HasPct   bool // the column was non-NULL
	// Valid is usage_observations.*_pct_valid: false marks a reading the
	// misparse classifier rejected, which must not anchor anything.
	Valid bool
	// Saturated marks a reading at the cap, where the meter stops moving
	// while tokens keep being spent. Deltas across it under-count.
	Saturated bool
	// ResetTS is the advertised reset, RFC3339, or "" when unparsed. This
	// identifies windows far more reliably than the reset-detected flags,
	// which are tuned for breaking chart lines and over-fire on misparses.
	ResetTS string
}

// Turn is one assistant turn's contribution.
type Turn struct {
	TSUnixMS    int64
	SessionUUID string
	Project     string
	RawTokens   int64
	CWTokens    float64
}

// Params configures one bucket's reconstruction.
type Params struct {
	Bucket string
	// Period is the limit window's length, used only to synthesize windows
	// for turns that predate the observation series.
	Period    time.Duration
	Ahead     time.Duration
	MinGap    time.Duration
	NowUnixMS int64
	// TokensPerPctCW is the calibration median for this bucket. Zero
	// disables the estimated fallback: uncovered turns still get a row
	// (with their token totals) but contribute no percentage.
	TokensPerPctCW float64
}

// ParamsFor returns the standard parameters for a bucket. now and the
// calibration median are the only per-call inputs.
func ParamsFor(bucket string, nowUnixMS int64, tokensPerPctCW float64) Params {
	p := Params{
		Bucket:         bucket,
		NowUnixMS:      nowUnixMS,
		TokensPerPctCW: tokensPerPctCW,
	}
	switch bucket {
	case BucketWeek:
		p.Period, p.Ahead, p.MinGap = WeekPeriod, WeekAhead, WeekMinGap
	default:
		p.Bucket = Bucket5h
		p.Period, p.Ahead, p.MinGap = SessPeriod, SessAhead, SessMinGap
	}
	return p
}

// Window is one reconstructed limit window.
type Window struct {
	Bucket        string
	StartUnixMS   int64
	EndUnixMS     int64
	ResetUnixMS   int64
	Inferred      bool // synthesized on a fixed grid; no observations covered it
	Partial       bool // the series began mid-window, so the total under-counts
	InProgress    bool // the newest window, still filling
	MeasuredPct   float64
	AttributedPct float64
	PeakPct       int
	HitCap        bool
	// TokensPerPctCW is what one point of this meter cost during this
	// window, reconciled over the whole window rather than assumed. Zero
	// when the window did not move enough to price anything.
	TokensPerPctCW float64
}

// Row is one session's share of one window.
type Row struct {
	Bucket            string
	WindowStartUnixMS int64
	SessionUUID       string
	Project           string
	MeasuredPct       float64
	EstimatedPct      float64
	CWTokens          float64
	RawTokens         int64
	TurnCount         int
	FirstTSUnixMS     int64
	LastTSUnixMS      int64
}

// Pct is the row's total share of its window.
func (r Row) Pct() float64 { return r.MeasuredPct + r.EstimatedPct }

// Result is one bucket's full reconstruction.
type Result struct {
	Windows []Window
	Rows    []Row
	// TokensPerPctCW is the most recent price of one point of this meter,
	// reconciled from observed windows. Zero when nothing could be measured
	// and Params.TokensPerPctCW was the only conversion available.
	TokensPerPctCW float64
}

// minRatioPct is how far a window's meter must have moved before that
// window is allowed to price tokens for the estimated fallback. Below it the
// ratio is mostly integer-rounding noise.
const minRatioPct = 3.0

// capTolerance multiplies the rate-implied cap (see attributeMeasured)
// before it is enforced. The window's own TokensPerPctCW is an estimate
// reconciled once per window, not a fact about any one span inside it: a
// span's real cost-per-point legitimately drifts around that average with
// the mix of cache reads/writes and tool use in the turns that happen to
// fall in it. A tight cap would misfile that ordinary variance as
// unattributed, which is worse than the bug it fixes (that would manufacture
// phantom cross-machine spend instead of reporting it honestly as none).
// 2x gives a span room to run at double the window's blended rate before
// anything is treated as unexplained, which is generous enough that it
// should only ever bite when a span's own turns truly cannot account for
// the movement, e.g. a second machine on the same account.
const capTolerance = 2.0

// segment is a window still under construction, with the observations that
// fell inside it.
type segment struct {
	Window
	obs []Obs
	// measuredCW is the cost-weighted size of every turn the measured pass
	// covered here. Divided by MeasuredPct it is what a point of this meter
	// actually cost during this window.
	measuredCW float64
}

// Build reconstructs one bucket's limit windows and attributes each one's
// meter movement to sessions. obs and turns need not be sorted.
func Build(obs []Obs, turns []Turn, p Params) Result {
	res := Result{Windows: []Window{}, Rows: []Row{}}
	if len(obs) == 0 && len(turns) == 0 {
		return res
	}

	obs = append([]Obs(nil), obs...)
	sort.Slice(obs, func(i, j int) bool { return obs[i].TSUnixMS < obs[j].TSUnixMS })
	turns = append([]Turn(nil), turns...)
	sort.Slice(turns, func(i, j int) bool { return turns[i].TSUnixMS < turns[j].TSUnixMS })

	segs := segmentWindows(obs, p)

	acc := newAccumulator(p.Bucket)
	covered := make([]bool, len(turns))

	// Measured pass: walk each window's runs of usable readings and split
	// the movement they bracket.
	for _, sg := range segs {
		measureWindow(sg, turns, covered, acc)
	}

	// Estimated pass: everything the measured pass did not reach, priced by
	// what the meter actually charged in the nearest window we could
	// reconcile.
	prices := priceWindows(segs)
	estimateRest(segs, turns, covered, acc, p, prices)

	res = acc.result(segs)
	res.TokensPerPctCW = prices.latest(p.TokensPerPctCW)
	return res
}

// priceTimeline is what one point of the meter cost over time, one entry per
// window whose movement was large enough to price.
type priceTimeline struct {
	at    []int64
	rates []float64
}

// priceWindows collects each window's already-reconciled rate into a
// timeline the estimated pass can look up by time. The rate itself
// (sg.TokensPerPctCW) is computed by measureWindow the moment a window's own
// movement and covered spend are fully tallied, not here: the same rate is
// also what bounds a span's locally-attributable delta (see
// attributeMeasured), and that bound has to exist before the measured pass
// finishes attributing rows, which is earlier than this function runs.
//
// This deliberately does NOT reuse calibration_points' tokens-per-percent.
// That table only keeps observation pairs where the displayed integer
// happened to tick, and charges a whole point to whatever was spent in that
// one poll interval, so every flat interval's tokens are dropped and the
// ratio comes out several times too cheap. Reconciling a whole window keeps
// the numerator and denominator over the same span, which is the only way
// the two halves of this package agree with each other.
func priceWindows(segs []*segment) priceTimeline {
	var t priceTimeline
	for _, sg := range segs {
		if sg.TokensPerPctCW <= 0 {
			continue
		}
		t.at = append(t.at, sg.StartUnixMS)
		t.rates = append(t.rates, sg.TokensPerPctCW)
	}
	return t
}

// nearest returns the rate from the window closest in time to ts, so spend
// in an unmeasurable stretch is priced like the era it happened in rather
// than like the average of all history. fallback covers having nothing.
func (t priceTimeline) nearest(ts int64, fallback float64) float64 {
	if len(t.at) == 0 {
		return fallback
	}
	i := sort.Search(len(t.at), func(i int) bool { return t.at[i] >= ts })
	switch {
	case i == 0:
		return t.rates[0]
	case i == len(t.at):
		return t.rates[len(t.rates)-1]
	case ts-t.at[i-1] <= t.at[i]-ts:
		return t.rates[i-1]
	default:
		return t.rates[i]
	}
}

func (t priceTimeline) latest(fallback float64) float64 {
	if len(t.rates) == 0 {
		return fallback
	}
	return t.rates[len(t.rates)-1]
}

// segmentWindows cuts the observation series into limit windows at the
// points where the advertised reset jumps forward by a whole period.
func segmentWindows(obs []Obs, p Params) []*segment {
	var (
		segs      []*segment
		cur       *segment
		curReset  time.Time
		haveReset bool
	)
	for _, o := range obs {
		r, rok := ParseReset(o.ResetTS, o.TSUnixMS, p.Ahead)
		switch {
		case cur == nil:
			// The oldest window is partial: we joined it mid-flight, so
			// whatever was spent before the first reading is invisible to
			// the measured path.
			cur = &segment{Window: Window{Bucket: p.Bucket, StartUnixMS: o.TSUnixMS, Partial: true}}
			segs = append(segs, cur)
			curReset, haveReset = r, rok
		case !rok:
			// Unparseable reset: carry the current window forward.
		case !haveReset:
			curReset, haveReset = r, true
		case r.After(curReset.Add(p.MinGap)):
			// The old window ended the moment it advertised, which is a
			// sharper boundary than "the reading where we noticed".
			b := clampMS(curReset.UnixMilli(), cur.StartUnixMS+1, o.TSUnixMS)
			cur.EndUnixMS = b
			cur.ResetUnixMS = curReset.UnixMilli()
			cur = &segment{Window: Window{Bucket: p.Bucket, StartUnixMS: b}}
			segs = append(segs, cur)
			curReset = r
		}
		cur.obs = append(cur.obs, o)
	}
	if cur != nil {
		cur.InProgress = true
		cur.EndUnixMS = maxMS(p.NowUnixMS, cur.StartUnixMS+1)
		if haveReset {
			cur.ResetUnixMS = curReset.UnixMilli()
		}
	}
	return segs
}

// measuredSpan is one run's movement, tallied but not yet split between
// local turns and the unattributed sentinel. See measureWindow for why the
// split has to wait.
type measuredSpan struct {
	hi             int64 // end of the run, used as the unattributed row's timestamp
	delta, totalCW float64
	from, to       int // turn index range [from, to) this span covers
}

// measureWindow attributes one window's observed meter movement.
//
// Movement is taken over *runs* of readings rather than adjacent pairs.
// /usage reports whole percents, so on a five-minute poll a per-pair delta is
// mostly rounding: whichever session happened to be running when the integer
// ticked would collect a whole point while its neighbours collected nothing.
// A run reconciles just as exactly at its endpoints and averages the rounding
// out across everything inside it.
//
// A run breaks only at a *saturated* reading, where the meter is pinned and
// stops reflecting spend, so nothing measured across it could be trusted.
// Missing or misparsed readings do not break a run; they are simply denied
// the right to anchor it or move its peak, which is all their unreliability
// warrants.
//
// Movement is measured against the running peak, never the raw reading, so a
// dip the classifier failed to flag cannot manufacture spend when the meter
// "recovers".
//
// This walks the window's runs twice. The first pass tallies each run's
// delta and covered cost-weighted tokens into sg.MeasuredPct/sg.measuredCW,
// which is also everything the window needs to reconcile its own rate
// (sg.TokensPerPctCW, same formula as priceWindows). Only once every run in
// the window has contributed to that rate does the second pass actually
// split each run's delta between local turns and the unattributed sentinel,
// because the cap attributeMeasured applies is expressed in terms of that
// rate. A window's rate is a fact about the whole window, so it cannot be
// trusted until the whole window has been walked once.
func measureWindow(sg *segment, turns []Turn, covered []bool, acc *accumulator) {
	for _, o := range sg.obs {
		if o.Saturated {
			sg.HitCap = true
		}
		if o.HasPct && o.Valid {
			if v := minInt(o.Pct, 100); v > sg.PeakPct {
				sg.PeakPct = v
			}
		}
	}

	var spans []measuredSpan
	var (
		havePeak bool
		peak     int
		firstRun = true
	)
	for i := 0; i < len(sg.obs); {
		if sg.obs[i].Saturated {
			i++
			continue
		}
		j := i
		for j+1 < len(sg.obs) && !sg.obs[j+1].Saturated {
			j++
		}

		lo, hi, runPeak, ok := anchors(sg.obs[i : j+1])
		if !ok {
			// Nothing in this stretch can anchor a measurement; its turns
			// fall through to the estimated pass.
			i = j + 1
			continue
		}

		// A window opened by a real reset starts the meter at zero, so its
		// first run baselines at zero and reaches back to the window start.
		// That is what captures spend between the reset and the window's
		// first poll; otherwise it would be invisible, since the rise is
		// already baked into that first reading.
		entry := lo.Pct
		spanLo := lo.TSUnixMS
		if firstRun && !sg.Partial {
			entry = 0
			spanLo = sg.StartUnixMS - 1
		} else if havePeak && peak > entry {
			entry = peak
		}
		if runPeak < entry {
			runPeak = entry
		}
		peak, havePeak = runPeak, true

		delta := float64(runPeak - entry)
		from, to := rangeIn(turns, spanLo, hi.TSUnixMS)
		var totalCW float64
		for k := from; k < to; k++ {
			totalCW += turns[k].CWTokens
			covered[k] = true
		}
		sg.MeasuredPct += delta
		sg.measuredCW += totalCW
		spans = append(spans, measuredSpan{hi: hi.TSUnixMS, delta: delta, totalCW: totalCW, from: from, to: to})

		firstRun = false
		i = j + 1
	}

	// The window's rate, reconciled exactly the way priceWindows describes.
	// Below minRatioPct the movement is mostly rounding noise, so no rate is
	// trusted and sg.TokensPerPctCW stays zero, which tells attributeMeasured
	// there is no cap to apply here (requirement: a window that can't price
	// itself behaves exactly as it did before this cap existed).
	if sg.MeasuredPct >= minRatioPct && sg.measuredCW > 0 {
		sg.TokensPerPctCW = round2(sg.measuredCW / sg.MeasuredPct)
	}

	for _, s := range spans {
		acc.attributeMeasured(sg, s.hi, s.delta, s.totalCW, s.from, s.to, turns)
	}
}

// anchors picks the first and last readings in a stretch that are allowed to
// bound a measurement, along with the highest value any of them reached. ok
// is false when the stretch contains no usable reading at all.
func anchors(obs []Obs) (lo, hi Obs, peak int, ok bool) {
	for _, o := range obs {
		if !usable(o) {
			continue
		}
		v := minInt(o.Pct, 100)
		if !ok {
			lo, peak, ok = o, v, true
		}
		hi = o
		if v > peak {
			peak = v
		}
	}
	lo.Pct = minInt(lo.Pct, 100)
	return lo, hi, peak, ok
}

// estimateRest converts every turn the measured pass did not reach into a
// percentage via the calibration median, and files it under the window that
// contains it. Turns older than the observation series get a window
// synthesized on a fixed grid stepping back from the oldest real one.
func estimateRest(segs []*segment, turns []Turn, covered []bool, acc *accumulator, p Params, prices priceTimeline) {
	periodMS := int64(p.Period / time.Millisecond)
	if periodMS <= 0 {
		periodMS = int64(SessPeriod / time.Millisecond)
	}

	// Anchor for the synthetic grid: the oldest real window if we have one,
	// otherwise the oldest turn (an install that has never polled /usage
	// still gets a complete, if wholly estimated, breakdown).
	var anchor int64
	if len(segs) > 0 {
		anchor = segs[0].StartUnixMS
	} else if len(turns) > 0 {
		anchor = turns[0].TSUnixMS
	}

	for i, t := range turns {
		if covered[i] {
			continue
		}
		var win *Window
		if sg := findSegment(segs, t.TSUnixMS); sg != nil {
			win = &sg.Window
		} else {
			win = acc.syntheticWindow(gridStart(t.TSUnixMS, anchor, periodMS), periodMS, p.Bucket)
		}
		var pct float64
		if rate := prices.nearest(t.TSUnixMS, p.TokensPerPctCW); rate > 0 {
			pct = t.CWTokens / rate
		}
		acc.add(win.StartUnixMS, t, 0, pct)
	}
}

// gridStart places ts on a fixed grid of period-long windows aligned to
// anchor. Real 5h windows are usage-triggered rather than clock-aligned, so
// for that bucket this is a coarse stand-in, which is exactly why every
// window it produces is flagged inferred.
func gridStart(ts, anchor, periodMS int64) int64 {
	d := anchor - ts
	if d <= 0 {
		return anchor + (ts-anchor)/periodMS*periodMS
	}
	k := (d + periodMS - 1) / periodMS
	return anchor - k*periodMS
}

// findSegment returns the observation-derived window containing ts, or nil
// when ts predates the series. Windows are contiguous by construction and
// the newest one runs to now, so a hit is a plain predecessor search.
func findSegment(segs []*segment, ts int64) *segment {
	if len(segs) == 0 || ts < segs[0].StartUnixMS {
		return nil
	}
	i := sort.Search(len(segs), func(i int) bool { return segs[i].StartUnixMS > ts })
	return segs[i-1]
}

// accumulator collects per-(window, session) rows plus the synthetic windows
// the estimated pass invents.
type accumulator struct {
	bucket    string
	rows      map[int64]map[string]*Row
	synthetic map[int64]*Window
}

func newAccumulator(bucket string) *accumulator {
	return &accumulator{
		bucket:    bucket,
		rows:      map[int64]map[string]*Row{},
		synthetic: map[int64]*Window{},
	}
}

func (a *accumulator) syntheticWindow(start, periodMS int64, bucket string) *Window {
	if w, ok := a.synthetic[start]; ok {
		return w
	}
	w := &Window{
		Bucket:      bucket,
		StartUnixMS: start,
		EndUnixMS:   start + periodMS,
		Inferred:    true,
	}
	a.synthetic[start] = w
	return w
}

// attributeMeasured splits delta across the turns[from:to) that fell in this
// run, in proportion to their cost-weighted size, but caps what those turns
// can collectively be charged: at most capTolerance times what totalCW is
// worth at the window's own reconciled rate (sg.TokensPerPctCW, already
// fixed for the whole window by measureWindow's first pass before this
// runs). Whatever delta exceeds that cap is filed under the unattributed
// sentinel instead of being smeared over turns that cannot plausibly account
// for it -- the case this guards is a second machine on the same account
// contributing real movement inside a span that also happens to hold a small
// local turn, which would otherwise be charged for all of it.
//
// When sg.TokensPerPctCW is zero (the window never moved enough to price
// itself; see measureWindow) no cap can be computed, so the entire delta is
// attributed exactly as it was before this cap existed.
//
// Turns land in the row set either way (even the capped share of a turn is
// real, recorded spend), a span the meter did not visibly move through still
// spent tokens worth recording.
func (a *accumulator) attributeMeasured(
	sg *segment, hi int64, delta, totalCW float64, from, to int, turns []Turn,
) {
	if totalCW <= 0 {
		// The meter moved but nothing we ingested explains it.
		if delta > 0 {
			a.add(sg.StartUnixMS, Turn{TSUnixMS: hi}, delta, 0)
		}
		return
	}

	local := delta
	if sg.TokensPerPctCW > 0 {
		if cap := capTolerance * totalCW / sg.TokensPerPctCW; cap < local {
			local = cap
		}
	}
	for i := from; i < to; i++ {
		a.add(sg.StartUnixMS, turns[i], local*turns[i].CWTokens/totalCW, 0)
	}
	if excess := delta - local; excess > 0 {
		a.add(sg.StartUnixMS, Turn{TSUnixMS: hi}, excess, 0)
	}
}

func (a *accumulator) add(windowStart int64, t Turn, measured, estimated float64) {
	byUUID, ok := a.rows[windowStart]
	if !ok {
		byUUID = map[string]*Row{}
		a.rows[windowStart] = byUUID
	}
	r, ok := byUUID[t.SessionUUID]
	if !ok {
		r = &Row{
			Bucket:            a.bucket,
			WindowStartUnixMS: windowStart,
			SessionUUID:       t.SessionUUID,
			Project:           t.Project,
			FirstTSUnixMS:     t.TSUnixMS,
		}
		byUUID[t.SessionUUID] = r
	}
	r.MeasuredPct += measured
	r.EstimatedPct += estimated
	if r.FirstTSUnixMS == 0 || t.TSUnixMS < r.FirstTSUnixMS {
		r.FirstTSUnixMS = t.TSUnixMS
	}
	if t.TSUnixMS > r.LastTSUnixMS {
		r.LastTSUnixMS = t.TSUnixMS
	}
	// The unattributed sentinel carries percentage only: by definition no
	// turn of ours backs it, so counting tokens against it would be a lie.
	if t.SessionUUID == Unattributed {
		return
	}
	r.CWTokens += t.CWTokens
	r.RawTokens += t.RawTokens
	r.TurnCount++
}

// result flattens the accumulated state, rounding once at the edge so the
// stored numbers match what the UI renders.
func (a *accumulator) result(segs []*segment) Result {
	windows := make([]Window, 0, len(segs)+len(a.synthetic))
	for _, sg := range segs {
		windows = append(windows, sg.Window)
	}
	for _, w := range a.synthetic {
		windows = append(windows, *w)
	}

	rows := make([]Row, 0, len(a.rows))
	total := map[int64]float64{}
	for start, byUUID := range a.rows {
		for _, r := range byUUID {
			r.MeasuredPct = round4(r.MeasuredPct)
			r.EstimatedPct = round4(r.EstimatedPct)
			r.CWTokens = round2(r.CWTokens)
			total[start] += r.Pct()
			rows = append(rows, *r)
		}
	}
	for i := range windows {
		windows[i].MeasuredPct = round4(windows[i].MeasuredPct)
		windows[i].AttributedPct = round4(total[windows[i].StartUnixMS])
	}

	sort.Slice(windows, func(i, j int) bool { return windows[i].StartUnixMS < windows[j].StartUnixMS })
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].WindowStartUnixMS != rows[j].WindowStartUnixMS {
			return rows[i].WindowStartUnixMS < rows[j].WindowStartUnixMS
		}
		return rows[i].SessionUUID < rows[j].SessionUUID
	})
	return Result{Windows: windows, Rows: rows}
}

// ParseReset parses and validates an advertised reset timestamp. ok is false
// when the reset is missing, unparseable, already behind the reading, or
// implausibly far ahead of it: all parse artifacts, and the caller should
// carry the previous window forward rather than split on junk.
func ParseReset(resetTS string, tsMS int64, maxAhead time.Duration) (time.Time, bool) {
	if resetTS == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, resetTS)
	if err != nil {
		return time.Time{}, false
	}
	obs := time.UnixMilli(tsMS)
	if t.Before(obs) || t.After(obs.Add(maxAhead)) {
		return time.Time{}, false
	}
	return t, true
}

// usable reports whether a reading can anchor a delta: present, not rejected
// by the misparse classifier, and not pinned at the cap.
func usable(o Obs) bool { return o.HasPct && o.Valid && !o.Saturated }

// rangeIn returns the [from, to) index range of turns in the half-open
// interval (lo, hi].
func rangeIn(turns []Turn, lo, hi int64) (int, int) {
	from := sort.Search(len(turns), func(i int) bool { return turns[i].TSUnixMS > lo })
	to := sort.Search(len(turns), func(i int) bool { return turns[i].TSUnixMS > hi })
	if to < from {
		to = from
	}
	return from, to
}

func clampMS(v, lo, hi int64) int64 {
	if hi < lo {
		hi = lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func maxMS(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }
func round4(f float64) float64 { return math.Round(f*10000) / 10000 }
