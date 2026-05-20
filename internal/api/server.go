// Package api wires the HTTP routes for the daemon. The API is JSON-only;
// the React bundle is served separately by the bloodhound-gui binary,
// which proxies /api/* back here.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/store"
	"github.com/PeterSR/claude-code-bloodhound/internal/version"
)

// Server bundles the deps the HTTP handlers need.
type Server struct {
	Store *store.Store
}

// Handler returns the root http.Handler. Anything outside /api/* is 404 —
// the daemon does not serve a UI.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/doctor", s.handleDoctor)
	mux.HandleFunc("/api/now", s.handleNow)
	mux.HandleFunc("/api/debug", s.handleDebug)
	mux.HandleFunc("/api/sessions", s.handleSessions)
	mux.HandleFunc("/api/sessions/", s.handleSessionDetail)
	mux.HandleFunc("/api/history", s.handleHistory)
	mux.HandleFunc("/api/compactions", s.handleCompactions)
	mux.HandleFunc("/api/leaks", s.handleLeaks)
	mux.HandleFunc("/api/settings", s.handleSettings)

	return logger(mux)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"version": version.Version,
	})
}

func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	v, err := s.Store.SchemaVersion(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}
	id, err := s.Store.DeviceID(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"version":        version.Version,
		"schema_version": v,
		"device_id":      id,
		"db_path":        s.Store.Path,
	})
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func logger(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t0 := time.Now()
		h.ServeHTTP(w, r)
		_ = t0 // hook left in place; verbose logging configurable later
	})
}

// Run is a small convenience for cmd/bloodhound to spin up the server.
func Run(ctx context.Context, host string, port int, s *Server) error {
	addr := fmt.Sprintf("%s:%d", host, port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		fmt.Fprintf(stderr(), "[api] http://%s/\n", addr)
		err := srv.ListenAndServe()
		if !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		return err
	}
}
