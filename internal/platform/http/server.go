// Package http provides the process HTTP server with health endpoints and
// graceful shutdown. It carries no business routes; modules contribute
// handlers through the composition root in cmd.
package http

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// ReadinessCheck reports whether a dependency is ready to serve traffic.
type ReadinessCheck func(ctx context.Context) error

// Server wraps the standard library HTTP server with health endpoints and
// context-driven graceful shutdown.
type Server struct {
	log   *slog.Logger
	inner *http.Server
}

// New builds a Server listening on addr. The ready check backs /readyz;
// pass the database ping. A nil check reports always ready.
func New(log *slog.Logger, addr string, ready ReadinessCheck) *Server {
	s := &Server{log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz(ready))
	s.inner = &http.Server{
		Addr:              addr,
		Handler:           noindex(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

// noindex stamps every response with a robots-blocking header. The API is
// never a crawlable surface, whatever the deployment topology puts in
// front of it - defence in depth behind the single frontend enforcement
// point.
func noindex(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Robots-Tag", "noindex, nofollow")
		next.ServeHTTP(w, r)
	})
}

// Handler exposes the route table, primarily for tests.
func (s *Server) Handler() http.Handler { return s.inner.Handler }

// Run serves until ctx is cancelled, then shuts down gracefully. It returns
// the listen error, or the shutdown error after cancellation.
func (s *Server) Run(ctx context.Context) error {
	listenErr := make(chan error, 1)
	go func() {
		if err := s.inner.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			listenErr <- err
		}
	}()
	select {
	case err := <-listenErr:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.inner.Shutdown(shutdownCtx)
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	s.writeStatus(w, http.StatusOK, "ok")
}

func (s *Server) handleReadyz(ready ReadinessCheck) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ready != nil {
			if err := ready(r.Context()); err != nil {
				s.log.WarnContext(r.Context(), "readiness check failed", "error", err)
				s.writeStatus(w, http.StatusServiceUnavailable, "unavailable")
				return
			}
		}
		s.writeStatus(w, http.StatusOK, "ok")
	}
}

func (s *Server) writeStatus(w http.ResponseWriter, code int, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if _, err := w.Write([]byte(`{"status":"` + status + `"}`)); err != nil {
		s.log.Warn("writing health response", "error", err)
	}
}
