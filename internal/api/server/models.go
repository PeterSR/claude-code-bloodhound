package server

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
	"github.com/PeterSR/claude-code-bloodhound/internal/costweight"
	"github.com/PeterSR/claude-code-bloodhound/internal/sessioninsight"
)

// divergenceTol is how far a measured weight may sit from list price
// before the UI flags the row. Set well above the method's own noise so
// a flag means something.
const divergenceTol = 0.25

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	acct, ok := s.withAccount(w, r)
	if !ok {
		return
	}

	days := clampQueryInt(r, "window_days", 30, 1, 365)
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()

	// weights_days scopes the weight estimate independently of the
	// attribution window. They are different questions: attribution is
	// happy to look back a year, while a weight measured across model eras
	// is actively misleading.
	weightsDays := clampQueryInt(r, "weights_days", 30, 1, 365)
	weightsSince := time.Now().Add(-time.Duration(weightsDays) * 24 * time.Hour).UnixMilli()

	out := routes.ModelsResponse{OK: true, WindowDays: days}
	out.Prices = priceTableInfo()

	// Per-model totals over the window, split into priced and unpriced.
	// An unpriced model has no honest cost-weighted share, so it is held
	// out of the attribution (shown separately in raw terms) rather than
	// folded in at the fallback rate.
	totals, err := s.modelTotals(ctx, acct, cutoff)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	out.Models = []routes.ModelTotal{}
	out.Unpriced = []routes.ModelTotal{}
	for _, t := range totals {
		if t.Known {
			out.TotalCWTokens += t.CWTokens
			out.Models = append(out.Models, t)
		} else {
			out.Unpriced = append(out.Unpriced, t)
		}
	}
	for i := range out.Models {
		if out.TotalCWTokens > 0 {
			out.Models[i].Share = round4(out.Models[i].CWTokens / out.TotalCWTokens)
		}
	}
	out.TotalCWTokens = round2(out.TotalCWTokens)

	// Daily attribution.
	out.Days, err = s.modelDays(ctx, acct, cutoff)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	// Learned weights. The estimator's window is its own, so the models it
	// reports on are those used in that window rather than the attribution
	// window's. Only priced models are compared — the weights table is
	// list-vs-measured, and an unpriced model has no list side.
	weightModels := totals
	if weightsSince != cutoff {
		// Prices are per model, not per account, so the weight estimate
		// pools every account's turns.
		if weightModels, err = s.modelTotals(ctx, 0, weightsSince); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
	}
	priced := make([]routes.ModelTotal, 0, len(weightModels))
	for _, t := range weightModels {
		if t.Known {
			priced = append(priced, t)
		}
	}
	out.Weights, err = s.modelWeights(ctx, weightsSince, weightsDays, priced)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, out)
}

// acct 0 means every account.
func (s *Server) modelTotals(ctx context.Context, acct, cutoff int64) ([]routes.ModelTotal, error) {
	// HAVING cw > 0 drops pseudo-models that never spent anything. Claude
	// Code writes "<synthetic>" as the model on messages it generates
	// itself (API errors, interrupts); every token column on those rows is
	// zero, so they cost nothing and have no place in a spend breakdown.
	rows, err := s.Store.DB.QueryContext(ctx, `
		SELECT model,
		       COUNT(*) AS turns,
		       COALESCE(SUM(`+sessioninsight.RawExpr+`), 0) AS raw,
		       COALESCE(SUM(`+sessioninsight.CWExpr()+`), 0) AS cw
		FROM turns
		WHERE ts_unix_ms >= ? AND (? = 0 OR account_id = ?)
		GROUP BY model
		HAVING cw > 0
	`, cutoff, acct, acct)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Empty means empty, not null: these arrays are mapped over directly by
	// the client, so a nil slice would serialize to null and crash it.
	out := []routes.ModelTotal{}
	for rows.Next() {
		var t routes.ModelTotal
		if err := rows.Scan(&t.Model, &t.Turns, &t.RawTokens, &t.CWTokens); err != nil {
			return nil, err
		}
		t.Multiplier = costweight.Multiplier(t.Model)
		t.Known = costweight.Known(t.Model)
		if p, ok := costweight.Current().Price(t.Model); ok {
			t.InputPrice = p.Input
			t.OutputPrice = p.Output
			t.Unverified = p.Status == costweight.PriceStatusUnverified
			t.Source = p.Source
		}
		t.BreaksRatio = costweight.Current().BreaksRatioAssumption(t.Model)
		t.CWTokens = round2(t.CWTokens)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CWTokens > out[j].CWTokens })
	return out, nil
}

// modelDays buckets spend into local calendar days. Bucketing happens in
// Go rather than SQL so day boundaries follow the user's timezone (and its
// DST shifts) instead of UTC.
func (s *Server) modelDays(ctx context.Context, acct, cutoff int64) ([]routes.ModelDay, error) {
	rows, err := s.Store.DB.QueryContext(ctx, `
		SELECT ts_unix_ms, model, `+sessioninsight.CWExpr()+` AS cw
		FROM turns
		WHERE ts_unix_ms >= ? AND account_id = ?
	`, cutoff, acct)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byDay := map[int64]map[string]float64{}
	for rows.Next() {
		var (
			tsMS  int64
			model string
			cw    float64
		)
		if err := rows.Scan(&tsMS, &model, &cw); err != nil {
			return nil, err
		}
		if !costweight.Known(model) {
			// Unpriced models are held out of the cost-weighted
			// attribution, so they don't belong on the daily burn chart
			// either — including them would make the priced segments fail
			// to sum to the bar.
			continue
		}
		t := time.UnixMilli(tsMS)
		day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()).UnixMilli()
		if byDay[day] == nil {
			byDay[day] = map[string]float64{}
		}
		byDay[day][model] += cw
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]routes.ModelDay, 0, len(byDay))
	for day, models := range byDay {
		d := routes.ModelDay{TSUnixMS: day, CWTokens: map[string]float64{}}
		for m, cw := range models {
			d.CWTokens[m] = round2(cw)
			d.TotalCW += cw
		}
		d.TotalCW = round2(d.TotalCW)
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TSUnixMS < out[j].TSUnixMS })
	return out, nil
}

// modelWeights measures each model against the baseline. present lists the
// models actually used in the same window, so models the estimator cannot
// reach still get a row: a model used only alongside others (subagent work,
// typically) never dominates a window and so can never be measured, and a
// silently missing row would read as a bug rather than as the limitation
// it is.
func (s *Server) modelWeights(
	ctx context.Context, since int64, days int, present []routes.ModelTotal,
) (routes.WeightsReport, error) {
	rep := routes.WeightsReport{
		Rows:        []routes.ModelWeight{},
		Baseline:    weightBaseline,
		SinceUnixMS: since,
		Days:        days,
		Bucket:      "session",
		MinSamples:  costweight.MinSamples,
	}

	obs, err := costweight.Learn(ctx, s.Store.DB, costweight.LearnOpts{
		Bucket:      rep.Bucket,
		SinceUnixMS: since,
		Baseline:    rep.Baseline,
	})
	if err != nil {
		return rep, err
	}

	measured := map[string]costweight.Observed{}
	for _, o := range obs {
		measured[o.Model] = o
		if o.IsBaseline && o.Samples >= costweight.MinSamples {
			rep.BaselineOK = true
		}
	}

	// Ordered by spend, matching the attribution table above it, so the
	// model that dominates the burn is the row the eye lands on first.
	for _, t := range present {
		o, ok := measured[t.Model]
		if !ok {
			o = costweight.Observed{
				Model:      t.Model,
				ListWeight: costweight.Multiplier(t.Model),
				IsBaseline: t.Model == rep.Baseline,
			}
		}
		rep.Rows = append(rep.Rows, routes.ModelWeight{
			Model:        o.Model,
			Samples:      o.Samples,
			TokensPerPct: round2(o.TokensPerPct),
			Weight:       round2(o.Weight),
			ListWeight:   o.ListWeight,
			IsBaseline:   o.IsBaseline,
			Diverges:     o.Diverges(divergenceTol),
		})
	}

	// Any two models that list at the same price form a control pair: a
	// correct estimator must put them 1.00x apart. Report the first pair
	// with enough samples on both sides.
	for i := 0; i < len(obs) && rep.Control == nil; i++ {
		for j := i + 1; j < len(obs); j++ {
			ratio, ok := costweight.ControlError(obs, obs[i].Model, obs[j].Model)
			if !ok {
				continue
			}
			rep.Control = &routes.ControlPair{
				A: obs[i].Model, B: obs[j].Model, Ratio: round2(ratio),
			}
			break
		}
	}
	return rep, nil
}

// weightBaseline anchors the weight scale. Everything else is measured
// against it, so it must be a model the user actually runs; if it goes
// unused the whole report degrades to "not enough data" rather than
// reporting something wrong.
const weightBaseline = "claude-opus-4-8"

func clampQueryInt(r *http.Request, key string, def, lo, hi int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return def
	}
	return n
}

func round4(f float64) float64 {
	return float64(int64(f*10000+0.5)) / 10000
}

// priceTableInfo describes the active price table for the Models page.
func priceTableInfo() routes.PriceTableInfo {
	t := costweight.Current()
	return routes.PriceTableInfo{
		Origin:      string(t.Origin()),
		Version:     t.Version,
		Baseline:    t.Baseline,
		GeneratedAt: t.GeneratedAt,
		GeneratedBy: t.GeneratedBy,
		PricedCount: len(t.Models),
	}
}
