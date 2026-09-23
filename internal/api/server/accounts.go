package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
)

// accountParam is the account a meter-wide request is about: ?account=<id>,
// or the primary account when absent. Every account has its own meter, so a
// handler that reads one must be told which.
func (s *Server) accountParam(r *http.Request) (int64, error) {
	if v := r.URL.Query().Get("account"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			return 0, fmt.Errorf("bad account %q", v)
		}
		return id, nil
	}
	return s.Store.PrimaryAccountID(r.Context())
}

// withAccount resolves accountParam or answers 400 and reports false.
func (s *Server) withAccount(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := s.accountParam(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return 0, false
	}
	return id, true
}

func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost:
		var req routes.AccountLabelRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if err := s.Store.SetAccountLabel(ctx, req.ID, req.Label); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": err.Error()})
			return
		}
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	accts, err := s.Store.ListAccounts(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	primary, _ := s.Store.PrimaryAccountID(ctx)
	metered, _ := s.Store.MeteredAccountIDs(ctx)
	logins, _ := s.Store.CurrentLogins(ctx)

	out := routes.AccountsResponse{OK: true, PrimaryID: primary, Accounts: []routes.AccountInfo{}}
	for _, a := range accts {
		info := routes.AccountInfo{
			ID: a.ID, Name: a.DisplayName(), Label: a.Label,
			Email: a.Email, OrgName: a.OrgName, RateLimitTier: a.RateLimitTier,
			LastSeenMS: a.LastSeenMS,
		}
		for _, m := range metered {
			// MeteredAccountIDs always leads with the primary, reading or not.
			if m == a.ID && (m != primary || hasReading(r, s, m)) {
				info.Metered = true
			}
		}
		for _, l := range logins {
			if l.AccountID == a.ID {
				info.ConfigDirs = append(info.ConfigDirs, l.ConfigDir)
			}
		}
		out.Accounts = append(out.Accounts, info)
	}
	writeJSON(w, http.StatusOK, out)
}

func hasReading(r *http.Request, s *Server, id int64) bool {
	obs, err := s.Store.LatestUsage(r.Context(), id)
	return err == nil && obs != nil
}
