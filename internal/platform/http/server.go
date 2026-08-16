// Package http provides the process HTTP server with health endpoints and
// graceful shutdown. It carries no business routes; modules contribute
// handlers through the composition root in cmd.
package http

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/Nomos-N4s/apivo-news/api"
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
	log     *slog.Logger
	version string
	mux     *http.ServeMux
	inner   *http.Server
}

// New builds a Server listening on addr. The ready check backs /readyz;
// pass the database ping. A nil check reports always ready. Any routes
// given here are mounted on the same mux, sharing the noindex stamping;
// Mount adds more later.
//
// version is the build's stamped release version, reported on both health
// payloads. It is a parameter rather than an option because a probe that
// cannot read which version answered cannot tell a rolled-forward
// deployment from the previous container still serving: every construction
// site must answer the question. An empty version reports no version at
// all - "unversioned" is a fact, not a value to invent.
func New(log *slog.Logger, addr, version string, ready ReadinessCheck, routes ...Route) *Server {
	s := &Server{log: log, version: version, mux: http.NewServeMux()}
	for pattern, handler := range s.builtin(ready) {
		s.mux.HandleFunc(pattern, handler)
	}
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

// builtin maps every route the platform serves itself - the health pair and
// the OpenAPI document - to its handler. New registers exactly this map and
// Patterns reports exactly its keys, so the route table and the list of
// routes cannot disagree about what exists.
func (s *Server) builtin(ready ReadinessCheck) map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"GET /healthz":             s.handleHealthz,
		"GET /readyz":              s.handleReadyz(ready),
		"GET /api/v1/openapi.json": s.handleOpenAPI,
	}
}

// Patterns lists the ServeMux patterns ("METHOD /path") every Server
// registers, sorted. The OpenAPI drift test reads it - together with the
// patterns each module reports - to check the served document against the
// routes that actually exist.
func Patterns() []string {
	var s Server
	return slices.Sorted(maps.Keys(s.builtin(nil)))
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

// handleOpenAPI serves the embedded OpenAPI document. The bytes are the
// committed api/openapi.json compiled into the binary, so what a client
// reads here is exactly what CI validated - there is no generation step in
// between that could serve something else. It is the one API route the
// platform owns: tooling asks for the description of the whole surface, not
// of whichever modules a given deployment happens to have mounted.
func (s *Server) handleOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(api.OpenAPIJSON()); err != nil {
		s.log.Warn("writing openapi document", "error", err)
	}
}

// healthBody is the probe payload: the status, and the version of the build
// that answered. The version is what makes a probe conclusive - a request
// that reaches a container still running the previous release gets a 200
// either way, and only the version distinguishes them (the release
// pipeline's probe asserts it against the tag it deployed, issue #119). It
// is omitted rather than blanked when the build carries no stamp, because
// an empty version is not a version.
type healthBody struct {
	Status  string `json:"status"`
	Version string `json:"version,omitempty"`
}

func (s *Server) writeStatus(w http.ResponseWriter, code int, status string) {
	// Marshalled rather than concatenated: the version arrives from the
	// build's -ldflags, so it is not this package's string to trust.
	body, err := json.Marshal(healthBody{Status: status, Version: s.version})
	if err != nil {
		s.log.Warn("encoding health response", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if _, err := w.Write(body); err != nil {
		s.log.Warn("writing health response", "error", err)
	}
}
