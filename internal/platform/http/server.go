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

// Route pairs a ServeMux pattern with the handler a module contributed.
// Modules build their handlers; the composition root in cmd passes them
// here. The platform stays ignorant of what the routes do.
type Route struct {
	// Pattern is a net/http ServeMux pattern, e.g. "/api/v1/editorial/".
	Pattern string
	// Handler serves every request matching Pattern.
	Handler http.Handler
}

// Server wraps the standard library HTTP server with health endpoints and
// context-driven graceful shutdown.
type Server struct {
	log   *slog.Logger
	mux   *http.ServeMux
	inner *http.Server
}

// New builds a Server listening on addr. The ready check backs /readyz;
// pass the database ping. A nil check reports always ready. Any routes
// given here are mounted on the same mux, sharing the noindex stamping;
// Mount adds more later.
func New(log *slog.Logger, addr string, ready ReadinessCheck, routes ...Route) *Server {
	s := &Server{log: log, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz(ready))
	for _, r := range routes {
		s.mux.Handle(r.Pattern, r.Handler)
	}
	s.inner = &http.Server{
		Addr:              addr,
		Handler:           noindex(s.mux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

// Mount registers a module's route table on the server. This is how the
// composition root in cmd contributes business routes: the module builds an
// http.Handler, cmd mounts it here. Call before Run; mounted routes inherit
// the server-wide headers (X-Robots-Tag on every response).
func (s *Server) Mount(pattern string, handler http.Handler) {
	s.mux.Handle(pattern, handler)
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
