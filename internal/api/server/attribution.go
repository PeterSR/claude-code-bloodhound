package server

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
	"github.com/PeterSR/claude-code-bloodhound/internal/attribute"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

// attrMaxSlices caps how many slices a single window ships. Beyond it the
// tail is folded into one "other" slice rather than truncated, so a stacked
// bar still adds up to the window's total.
//
// Set well above the eight colours the chart can actually distinguish: the
// client does the final fold, against each group's total across the whole
// range rather than per window, so a project keeps one colour instead of
// changing it whenever a busier week reshuffles the local ranking. Folding
// here too early would undo that: a group in the range's top eight would
// lose its colour in any window where it happened to rank low.
const attrMaxSlices = 20

func (s *Server) handleAttribution(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	bucket := attribute.BucketWeek
	if r.URL.Query().Get("bucket") == attribute.Bucket5h {
		bucket = attribute.Bucket5h
	}
	by := "project"
	switch r.URL.Query().Get("by") {
	case "session":
		by = "session"
	case "cwd":
		by = "cwd"
	}
	// per_window and window=current are additive, presentation-only knobs
	// over the same rows the plain rollup already fetches: neither changes
	// what attrGroups queries, only what this handler does with the result,
	// so they're read permissively (same style as by/bucket above) rather
	// than 400ing on an unrecognised value.
	perWindow := r.URL.Query().Get("per_window") != ""
	windowCurrent := r.URL.Query().Get("window") == "current"

	// A weekly view wants months; a 5h view wants days. Same knob, very
	// different useful defaults.
	defDays := 56
	if bucket == attribute.Bucket5h {
		defDays = 7
	}
	days := clampQueryInt(r, "window_days", defDays, 1, 365)
	since := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()

	out := routes.AttributionResponse{
		OK:         true,
		Bucket:     bucket,
		By:         by,
		WindowDays: days,
		Windows:    []routes.AttrWindow{},
		Projects:   []routes.AttrGroup{},
		Sessions:   []routes.AttrGroup{},
		Cwds:       []routes.AttrGroup{},
	}
	windows, err := s.Store.ListLimitWindows(ctx, bucket, since)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	// window=current narrows every query below to the single open window.
	// Found from the window_days-scoped list just fetched when that already
	// reaches far enough back; window_days describes how far the ordinary
	// (unfiltered) rollup looks, not a bound on how old the open window can
	// be, so a narrow window_days could otherwise miss a window that's
	// genuinely still open just because its start predates the cutoff.
	windowOpen := true
	if windowCurrent {
		out.Window = "current"
		curWindows := windows
		if curSince := currentWindowSince(bucket); curSince < since {
			if curWindows, err = s.Store.ListLimitWindows(ctx, bucket, curSince); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		}
		if cur := currentLimitWindow(curWindows); cur != nil {
			// A window's own start can never be in the future, so using it
			// as the new "since" is exactly "only this window": nothing
			// newer exists to leak in. No open window (aggregate hasn't run
			// yet, or genuinely nothing is open right now) degrades to an
			// honestly empty response rather than silently falling back to
			// the full range.
			since = cur.StartUnixMS
			windows = []store.LimitWindowRow{*cur}
		} else {
			windowOpen = false
			windows = []store.LimitWindowRow{}
		}
	}

	// Report the rate the estimate actually ran at: the newest window that
	// moved enough to price itself. The calibration median is only the
	// last-resort fallback, and it reads several times too cheap (it prices
	// a whole point against the single poll interval the displayed integer
	// happened to tick in), so quoting it here would misdescribe the number
	// on screen.
	for i := len(windows) - 1; i >= 0; i-- {
		if windows[i].TokensPerPctCW > 0 {
			out.TokensPerPctCW = round2(windows[i].TokensPerPctCW)
			break
		}
	}
	if out.TokensPerPctCW == 0 {
		if cw, _, _, ok, err := s.Store.LatestCalibrationMedian(ctx, calibrationBucketName(bucket), 10); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		} else if ok {
			out.TokensPerPctCW = round2(cw)
		}
	}

	var slices []store.AttributionRow
	if windowOpen {
		if slices, err = s.Store.WindowSlices(ctx, bucket, since); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
	}
	out.Windows = buildAttrWindows(windows, slices, by)

	if windowOpen {
		if out.Projects, err = s.attrGroups(ctx, bucket, "project", since); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if out.Sessions, err = s.attrGroups(ctx, bucket, "session", since); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if out.Cwds, err = s.attrGroups(ctx, bucket, "cwd", since); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
	}

	// per_window is attached to all three rollups regardless of "by" (same
	// "always populate all three" choice attrGroups' callers already made
	// above): it's cheap, since it's grouping data already fetched for
	// Windows above, not a new query per rollup.
	if perWindow {
		attachPerWindow(out.Projects, windows, slices, "project")
		attachPerWindow(out.Sessions, windows, slices, "session")
		attachPerWindow(out.Cwds, windows, slices, "cwd")
	}

	for _, g := range out.Projects {
		out.TotalPct += g.Pct
		out.MeasuredPct += g.MeasuredPct
		out.EstimatedPct += g.EstimatedPct
		if g.Key == attribute.Unattributed {
			out.UnattributedPct += g.Pct
		}
	}
	out.TotalPct = round2(out.TotalPct)
	out.MeasuredPct = round2(out.MeasuredPct)
	out.EstimatedPct = round2(out.EstimatedPct)
	out.UnattributedPct = round2(out.UnattributedPct)

	writeJSON(w, http.StatusOK, out)
}

// attrGroups converts one rollup into wire form, filling in each group's
// share of everything attributed in the range.
func (s *Server) attrGroups(ctx context.Context, bucket, by string, since int64) ([]routes.AttrGroup, error) {
	groups, err := s.Store.GroupAttribution(ctx, bucket, by, since)
	if err != nil {
		return nil, err
	}
	var total float64
	for _, g := range groups {
		total += g.Pct
	}
	out := make([]routes.AttrGroup, 0, len(groups))
	for _, g := range groups {
		row := routes.AttrGroup{
			Key:           g.Key,
			Project:       g.Project,
			Cwd:           g.Cwd,
			Pct:           round2(g.Pct),
			MeasuredPct:   round2(g.MeasuredPct),
			EstimatedPct:  round2(g.EstimatedPct),
			PeakPct:       round2(g.PeakPct),
			CWTokens:      round2(g.CWTokens),
			RawTokens:     g.RawTokens,
			TurnCount:     g.TurnCount,
			Sessions:      g.Sessions,
			Windows:       g.Windows,
			FirstTSUnixMS: g.FirstTSUnixMS,
			LastTSUnixMS:  g.LastTSUnixMS,
		}
		if total > 0 {
			row.Share = round4(g.Pct / total)
		}
		out = append(out, row)
	}
	return out, nil
}

// attrGroupKey derives the grouping key (and human label) one slice
// contributes under a "by" mode, exactly the way GroupAttribution's SQL
// derives the same group's key: shared by buildAttrWindows (one slice array
// per window) and buildAttrGroupWindows (one window array per group) so the
// two can't drift apart on what a key means, and so a key here always finds
// its matching group in Projects/Sessions/Cwds.
func attrGroupKey(sl store.AttributionRow, by string) (key, label string) {
	key, label = sl.EffectiveSessionUUID, sl.EffectiveSessionUUID
	if by == "project" {
		key, label = sl.Project, sl.Project
	}
	if by == "cwd" {
		key, label = sl.Cwd, sl.Cwd
	}
	switch {
	case sl.SessionUUID == attribute.Unattributed:
		// The remainder is neither a project, a session, nor a cwd; give
		// it one stable key across every grouping so the UI can style it
		// as the gap it is.
		key, label = attribute.Unattributed, ""
	case by == "cwd" && sl.Cwd == "":
		// A real, known session (or supervisor plus subagents) whose
		// cwd was never captured: distinguishable from the unattributed
		// remainder above, same UnknownCwd sentinel GroupAttribution's
		// by=="cwd" rollup uses for the identical row.
		key, label = store.UnknownCwd, store.UnknownCwd
	}
	return key, label
}

// currentWindowSince is how far back window=current is guaranteed to search
// for the open window, regardless of window_days: doubled past each
// bucket's real span as slack for a window that ran unusually long or a
// late aggregator pass, so the search only ever widens past whatever
// window_days already covers, never narrows it.
func currentWindowSince(bucket string) int64 {
	if bucket == attribute.Bucket5h {
		return time.Now().Add(-10 * time.Hour).UnixMilli()
	}
	return time.Now().Add(-14 * 24 * time.Hour).UnixMilli()
}

// currentLimitWindow returns the one in-progress window in a bucket's list,
// or nil when none is open (aggregate hasn't run since the last reset, or
// the requested lookback doesn't reach it). Walked from the end because
// ListLimitWindows/GroupAttribution both return oldest first, so the open
// window - if there is one - is always the last entry, not because
// "in progress" implies "most recent" in general.
func currentLimitWindow(windows []store.LimitWindowRow) *store.LimitWindowRow {
	for i := len(windows) - 1; i >= 0; i-- {
		if windows[i].InProgress {
			return &windows[i]
		}
	}
	return nil
}

// buildAttrGroupWindows is buildAttrWindows transposed: one array per GROUP
// (keyed exactly like attrGroupKey/GroupAttribution) instead of one array
// per window. This is what a rollup's Pct can't answer on its own once a
// range spans more than one window - Pct there sums every window in range,
// so it can say "how much this week" but never "how much of my current 5h
// budget" - and it's cheap because it reuses the same windows/slices data
// buildAttrWindows already has, rather than a second query.
func buildAttrGroupWindows(windows []store.LimitWindowRow, slices []store.AttributionRow, by string) map[string][]routes.AttrGroupWindow {
	type agg struct {
		measured, estimated, cw float64
		raw                     int64
		turns                   int
	}
	byWindow := map[int64]map[string]*agg{}
	for _, sl := range slices {
		key, _ := attrGroupKey(sl, by)
		m, ok := byWindow[sl.WindowStartUnixMS]
		if !ok {
			m = map[string]*agg{}
			byWindow[sl.WindowStartUnixMS] = m
		}
		a, ok := m[key]
		if !ok {
			a = &agg{}
			m[key] = a
		}
		a.measured += sl.MeasuredPct
		a.estimated += sl.EstimatedPct
		a.cw += sl.CWTokens
		a.raw += sl.RawTokens
		a.turns += sl.TurnCount
	}

	// windows is already oldest first (ListLimitWindows/WindowSlices' own
	// contract), so ranging over it in order, rather than sorting each key's
	// slice afterwards, is what keeps every group's PerWindow ascending too.
	out := map[string][]routes.AttrGroupWindow{}
	for _, win := range windows {
		for key, a := range byWindow[win.StartUnixMS] {
			out[key] = append(out[key], routes.AttrGroupWindow{
				WindowStartUnixMS: win.StartUnixMS,
				WindowEndUnixMS:   win.EndUnixMS,
				InProgress:        win.InProgress,
				Pct:               round2(a.measured + a.estimated),
				MeasuredPct:       round2(a.measured),
				EstimatedPct:      round2(a.estimated),
				CWTokens:          round2(a.cw),
				RawTokens:         a.raw,
				TurnCount:         a.turns,
			})
		}
	}
	return out
}

// attachPerWindow fills each group's PerWindow from the same window/slice
// data the stacked chart already computed for this response; groups is
// mutated in place since routes.AttrGroup is returned by value from
// attrGroups and this is the one place that needs to add to it afterwards.
func attachPerWindow(groups []routes.AttrGroup, windows []store.LimitWindowRow, slices []store.AttributionRow, by string) {
	perGroup := buildAttrGroupWindows(windows, slices, by)
	for i := range groups {
		groups[i].PerWindow = perGroup[groups[i].Key]
	}
}

// buildAttrWindows joins each window to its slices, regrouped by project, by
// session's effective owner, or by that same effective owner's cwd, and
// folds the long tail into one "other" entry so the stack still sums to the
// window total.
//
// by=="session" groups on EffectiveSessionUUID rather than the raw
// SessionUUID, so a subagent's slice folds into whoever dispatched it
// before this ever reaches foldTail. Without that, a subagent could show
// as its own slice in this stacked chart while GroupAttribution (the table
// directly beneath it) already folds that same spend into the parent, and
// the two would stop summing to the same number for the same window.
// by=="project" doesn't need this: a subagent already carries its parent's
// project (see internal/ingest), so grouping by project pools them
// naturally, same as GroupAttribution's comment notes for that case.
// by=="cwd" groups on Cwd (WindowSlices resolves it the same effective-owner
// way as EffectiveSessionUUID), with store.UnknownCwd standing in for a real
// session whose cwd was never captured, the same bucket GroupAttribution's
// by=="cwd" mode uses, so the chart and the table beneath it agree there too.
func buildAttrWindows(windows []store.LimitWindowRow, slices []store.AttributionRow, by string) []routes.AttrWindow {
	type agg struct {
		label                        string
		pct, measured, estimated, cw float64
		turns                        int
	}
	byWindow := map[int64]map[string]*agg{}
	for _, sl := range slices {
		key, label := attrGroupKey(sl, by)
		m, ok := byWindow[sl.WindowStartUnixMS]
		if !ok {
			m = map[string]*agg{}
			byWindow[sl.WindowStartUnixMS] = m
		}
		a, ok := m[key]
		if !ok {
			a = &agg{label: label}
			m[key] = a
		}
		a.pct += sl.MeasuredPct + sl.EstimatedPct
		a.measured += sl.MeasuredPct
		a.estimated += sl.EstimatedPct
		a.cw += sl.CWTokens
		a.turns += sl.TurnCount
	}

	out := make([]routes.AttrWindow, 0, len(windows))
	for _, w := range windows {
		aw := routes.AttrWindow{
			StartUnixMS:   w.StartUnixMS,
			EndUnixMS:     w.EndUnixMS,
			ResetUnixMS:   w.ResetUnixMS,
			Inferred:      w.Inferred,
			Partial:       w.Partial,
			InProgress:    w.InProgress,
			HitCap:        w.HitCap,
			MeasuredPct:   round2(w.MeasuredPct),
			AttributedPct: round2(w.AttributedPct),
			PeakPct:       w.PeakPct,
			Slices:        []routes.AttrSlice{},
		}
		for key, a := range byWindow[w.StartUnixMS] {
			aw.Slices = append(aw.Slices, routes.AttrSlice{
				Key:          key,
				Label:        a.label,
				Pct:          round2(a.pct),
				MeasuredPct:  round2(a.measured),
				EstimatedPct: round2(a.estimated),
				CWTokens:     round2(a.cw),
				TurnCount:    a.turns,
			})
		}
		sort.Slice(aw.Slices, func(i, j int) bool { return aw.Slices[i].Pct > aw.Slices[j].Pct })
		aw.Slices = foldTail(aw.Slices, attrMaxSlices)
		out = append(out, aw)
	}
	return out
}

// foldTail collapses everything past max into a single "other" slice.
func foldTail(slices []routes.AttrSlice, max int) []routes.AttrSlice {
	if len(slices) <= max {
		return slices
	}
	other := routes.AttrSlice{Key: "__other__", Label: "other"}
	for _, sl := range slices[max:] {
		other.Pct += sl.Pct
		other.MeasuredPct += sl.MeasuredPct
		other.EstimatedPct += sl.EstimatedPct
		other.CWTokens += sl.CWTokens
		other.TurnCount += sl.TurnCount
	}
	other.Pct = round2(other.Pct)
	other.MeasuredPct = round2(other.MeasuredPct)
	other.EstimatedPct = round2(other.EstimatedPct)
	other.CWTokens = round2(other.CWTokens)
	return append(slices[:max:max], other)
}

// calibrationBucketName maps an attribution bucket onto the name
// calibration_points uses, where the 5-hour window is called "session".
func calibrationBucketName(bucket string) string {
	if bucket == attribute.BucketWeek {
		return "week"
	}
	return "session"
}

// sessionAttribution converts a store rollup into the wire shape shared by
// the session list and the session detail page.
func sessionAttribution(t *store.SessionPctTotals) routes.SessionAttribution {
	if t == nil {
		return routes.SessionAttribution{}
	}
	return routes.SessionAttribution{
		Cwd:          t.Cwd,
		WeekPct:      round2(t.WeekPct),
		FiveHPct:     round2(t.FiveHPct),
		FiveHPeakPct: round2(t.FiveHPeakPct),
		MeasuredPct:  round2(t.MeasuredPct),
		EstimatedPct: round2(t.EstimatedPct),
		Windows5h:    t.Windows5h,
		WindowsWeek:  t.WindowWeek,
	}
}

// sessionWindowSlices walks one session's share of one bucket's windows.
func (s *Server) sessionWindowSlices(ctx context.Context, uuid, bucket string) ([]routes.SessionWindowSlice, error) {
	rows, wins, err := s.Store.SessionPctWindows(ctx, uuid, bucket)
	if err != nil {
		return nil, err
	}
	out := []routes.SessionWindowSlice{}
	for i, r := range rows {
		w := wins[i]
		out = append(out, routes.SessionWindowSlice{
			WindowStartUnixMS: r.WindowStartUnixMS,
			WindowEndUnixMS:   w.EndUnixMS,
			Inferred:          w.Inferred,
			Partial:           w.Partial,
			InProgress:        w.InProgress,
			Pct:               round2(r.MeasuredPct + r.EstimatedPct),
			MeasuredPct:       round2(r.MeasuredPct),
			EstimatedPct:      round2(r.EstimatedPct),
			WindowPct:         round2(w.AttributedPct),
			CWTokens:          round2(r.CWTokens),
			RawTokens:         r.RawTokens,
			TurnCount:         r.TurnCount,
			FirstTSUnixMS:     r.FirstTSUnixMS,
			LastTSUnixMS:      r.LastTSUnixMS,
		})
	}
	return out, nil
}
