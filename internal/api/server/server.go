// Package server implements the HTTP handlers behind the bloodhound
// daemon's API. The route paths and JSON payload types are described
// in sibling package internal/api/routes; the bloodhound-gui binary
// imports only routes (for the socket-path helper) so the GUI build
// doesn't pull in the daemon's pty / store / self-heal code.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
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

	mux.HandleFunc(routes.PathHealth, s.handleHealth)
	mux.HandleFunc(routes.PathDoctor, s.handleDoctor)
	mux.HandleFunc(routes.PathNow, s.handleNow)
	// Levels first: ServeMux matches the longer pattern, but stating the
	// order makes the intent obvious next to a prefix-shaped sibling.
	mux.HandleFunc(routes.PathEventsLevels, s.handleEventsLevels)
	mux.HandleFunc(routes.PathEvents, s.handleEvents)
	mux.HandleFunc(routes.PathDebug, s.handleDebug)
	mux.HandleFunc(routes.PathSessions, s.handleSessions)
	mux.HandleFunc(routes.PathSessionsPrefix, s.handleSessionDetail)
	mux.HandleFunc(routes.PathHistory, s.handleHistory)
	mux.HandleFunc(routes.PathCapacity, s.handleCapacity)
	mux.HandleFunc(routes.PathAttribution, s.handleAttribution)
	mux.HandleFunc(routes.PathModels, s.handleModels)
	mux.HandleFunc(routes.PathPricesHeal, s.handlePricesHeal)
	mux.HandleFunc(routes.PathCompactions, s.handleCompactions)
	mux.HandleFunc(routes.PathLeaks, s.handleLeaks)
	mux.HandleFunc(routes.PathSettings, s.handleSettings)
	mux.HandleFunc(routes.PathExtractorRetrain, s.handleExtractorRetrain)
	mux.HandleFunc(routes.PathTrailResolve, s.handleTrailResolve)
	mux.HandleFunc(routes.PathTrail, s.handleTrail)

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

// Serve runs the HTTP API on the supplied listener until ctx is cancelled.
// Caller owns listener creation (and cleanup, when applicable); this lets
// the daemon pick a unix socket while leaving the API package agnostic.
func Serve(ctx context.Context, ln net.Listener, s *Server) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		fmt.Fprintf(stderr(), "[api] listening on %s\n", ln.Addr())
		err := srv.Serve(ln)
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
