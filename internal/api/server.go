// Package api wires the HTTP routes for `bloodhound serve`. The web UI is
// served as a single-page app from an embedded bundle (see package web);
// API routes live under /api.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/store"
	"github.com/PeterSR/claude-code-bloodhound/internal/version"
	"github.com/PeterSR/claude-code-bloodhound/web"
)

// Server bundles the deps the HTTP handlers need.
type Server struct {
	Store *store.Store
}

// Handler returns the root http.Handler, with /api/* routed to JSON
// handlers and everything else served from the embedded SPA bundle.
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

	staticHandler := s.staticHandler()
	mux.Handle("/", staticHandler)

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
		"web_bundled":    web.Has(),
	})
}

// staticHandler serves the embedded SPA bundle. Unknown non-/api paths
// fall through to index.html so client-side routing works on hard reload.
// If the bundle is missing (fresh checkout, no `make web` yet) we render a
// helpful placeholder.
func (s *Server) staticHandler() http.Handler {
	if !web.Has() {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(placeholderHTML))
		})
	}
	sub, err := web.FS()
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, fmt.Sprintf("web: %v", err), http.StatusInternalServerError)
		})
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		// SPA fallback: anything that isn't a real file becomes index.html.
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			fileServer.ServeHTTP(w, r)
			return
		}
		if _, err := fs.Stat(sub, path); errors.Is(err, fs.ErrNotExist) {
			r2 := *r
			r2.URL.Path = "/"
			fileServer.ServeHTTP(w, &r2)
			return
		}
		fileServer.ServeHTTP(w, r)
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
		fmt.Fprintf(stderr(), "[serve] http://%s/\n", addr)
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

const placeholderHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Bloodhound — UI not built</title>
<style>
  body { font-family: ui-sans-serif, system-ui, -apple-system, sans-serif;
         background:#0c0c0c; color:#e6e6e6; margin:0; padding:48px;
         line-height:1.5; }
  h1 { font-size: 22px; margin: 0 0 8px; }
  p { color:#a0a0a0; max-width: 640px; }
  code { background:#222; padding:2px 6px; border-radius:4px; font-size: 13px; }
  a { color:#79c0ff; }
</style>
</head>
<body>
  <h1>Bloodhound</h1>
  <p>The Go binary is running, but no built frontend is bundled in.</p>
  <p>Build it with <code>make web</code> (or <code>cd web && npm install && npm run build</code>),
  then re-run <code>bloodhound serve</code>.</p>
  <p>If you just want to poke the API: <code>curl http://localhost:7777/api/health</code></p>
</body>
</html>`
