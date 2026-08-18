package server

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/budget"
	"github.com/PeterSR/claude-code-bloodhound/internal/events"
)

// budgetsResponse is what the Budgets page renders.
//
// Pressure travels with the budgets rather than as a second call. A budget
// without its current state is not actionable, and fetching the two separately
// lets the page show an allowance next to a state computed from a different
// moment.
type budgetsResponse struct {
	OK      bool              `json:"ok"`
	NowMS   int64             `json:"server_now_ms"`
	Budgets []budgetWithState `json:"budgets"`
}

type budgetWithState struct {
	budget.Budget
	// State is the recorded pressure level, "" when the reconciler has not
	// evaluated this budget yet (a budget set seconds ago, before the next
	// tick). The page shows that as "pending" rather than as clear.
	State string `json:"state,omitempty"`
	// SinceMS is when it entered that state, which is what lets the page say
	// "tight for 40m" instead of just "tight".
	SinceMS int64 `json:"since_ms,omitempty"`
	// InForce describes the lease keeping it alive, already rendered because
	// the phrasing rules live in Go and duplicating them in TypeScript is how
	// the two drift.
	InForce string `json:"in_force"`
}

// handleBudgets serves the budget list and accepts new ones.
//
//	GET  /api/budgets            every live budget with its current pressure
//	GET  /api/budgets?all=1      retired ones too
//	POST /api/budgets            set one
//	POST /api/budgets?revoke=1   retire one
func (s *Server) handleBudgets(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.getBudgets(w, r)
	case http.MethodPost:
		s.postBudget(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "GET or POST"})
	}
}

func (s *Server) getBudgets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	all := r.URL.Query().Get("all") != ""

	budgets, err := s.Store.ListBudgets(ctx, all)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	levels, err := events.Levels(ctx, s.Store.DB)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	type levelKey struct{ cwd, bucket string }
	byKey := map[levelKey]events.Level{}
	for _, l := range levels {
		if l.Kind == "budget" {
			byKey[levelKey{l.Scope.Cwd, l.Scope.Bucket}] = l
		}
	}

	now := time.Now()
	out := make([]budgetWithState, 0, len(budgets))
	for _, b := range budgets {
		row := budgetWithState{Budget: b, InForce: budgetInForce(b, now)}
		if l, ok := byKey[levelKey{b.Cwd, b.Bucket}]; ok {
			row.State, row.SinceMS = l.State, l.SinceMS
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, budgetsResponse{OK: true, NowMS: now.UnixMilli(), Budgets: out})
}

// budgetSetRequest is the POST body. Pct fields are omitted rather than zeroed
// to leave a rule unset, matching the CLI where a flag not passed is not a
// rule of zero.
type budgetSetRequest struct {
	Cwd      string  `json:"cwd"`
	Bucket   string  `json:"bucket"`
	SpendPct float64 `json:"spend_pct,omitempty"`
	MeterPct float64 `json:"meter_pct,omitempty"`
	Until    string  `json:"until,omitempty"`
	Note     string  `json:"note,omitempty"`
}

func (s *Server) postBudget(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req budgetSetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "malformed JSON: " + err.Error()})
		return
	}
	// Absolute only. NormalizeCwd would happily resolve a relative path with
	// filepath.Abs, but "here" for the daemon is wherever systemd started it,
	// which is never what a web client meant.
	if !filepath.IsAbs(strings.TrimSpace(req.Cwd)) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "error": "cwd must be an absolute path"})
		return
	}
	cwd, err := budget.NormalizeCwd(req.Cwd)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if req.Bucket == "" {
		req.Bucket = budget.BucketWeek
	}
	if !budget.ValidBucket(req.Bucket) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "error": "bucket must be \"session\" or \"week\""})
		return
	}
	now := time.Now()

	if r.URL.Query().Get("revoke") != "" {
		had, err := s.Store.RevokeBudget(ctx, cwd, req.Bucket, now)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "revoked": had})
		return
	}

	windowEnds, err := s.budgetWindowEnds(ctx, now)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	lease, err := budget.ParseUntil(req.Until, req.Bucket, now, windowEnds)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	saved, err := s.Store.SetBudget(ctx, budget.Budget{
		Cwd:      cwd,
		Bucket:   req.Bucket,
		SpendPct: req.SpendPct,
		MeterPct: req.MeterPct,
		Note:     req.Note,
		Leases:   []budget.Lease{lease},
	}, now)
	if err != nil {
		// Validate's messages are written for a person, so they are the
		// response rather than a generic 400.
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"budget": budgetWithState{Budget: saved, InForce: budgetInForce(saved, now)},
	})
}

func (s *Server) budgetWindowEnds(ctx context.Context, now time.Time) (map[string]int64, error) {
	out := map[string]int64{}
	for bucket, back := range map[string]time.Duration{
		budget.BucketSession: 10 * time.Hour,
		budget.BucketWeek:    14 * 24 * time.Hour,
	} {
		windows, err := s.Store.ListLimitWindows(ctx, budget.AttributeBucket(bucket), now.Add(-back).UnixMilli())
		if err != nil {
			return nil, err
		}
		for i := len(windows) - 1; i >= 0; i-- {
			if windows[i].InProgress {
				out[bucket] = windows[i].ResetUnixMS
				break
			}
		}
	}
	return out, nil
}

// normalizeCwdQuery matches a ?cwd= filter against the form budget scopes are
// stored in. Budget events key on budget.NormalizeCwd output, so a raw query
// value with a trailing slash compares unequal and matches nothing. A value
// that will not resolve is passed through rather than rejected, so this cannot
// break a filter on some future non-path scope.
func normalizeCwdQuery(v string) string {
	if v == "" {
		return ""
	}
	n, err := budget.NormalizeCwd(v)
	if err != nil {
		return v
	}
	return n
}

func budgetInForce(b budget.Budget, now time.Time) string {
	if b.RetiredMS != nil {
		why := b.RetiredWhy
		if why == "" {
			why = "retired"
		}
		return why
	}
	for _, l := range b.Leases {
		if l.ExpiredMS == nil {
			return l.Describe(now)
		}
	}
	return "no live lease"
}
