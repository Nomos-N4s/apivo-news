package main

// The served OpenAPI document and the router must describe the same API.
// This test fetches the document from a real server, parses it, and compares
// its operations with the patterns the modules register - in both
// directions, so neither an undocumented route nor a documented endpoint
// nobody serves can survive. It lives in the composition root because that
// is the only place where every module's route table is in scope (the arch
// test forbids the modules importing each other).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/content"
	"github.com/Nomos-N4s/apivo-news/internal/editorial"
	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// httpMethods are the path-item keys that denote an operation; anything else
// in a path item (parameters, summary, servers) is not one.
var httpMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

// openAPIDocument is the slice of the document this test reasons about.
type openAPIDocument struct {
	OpenAPI    string                                `json:"openapi"`
	Security   *[]map[string][]string                `json:"security"`
	Paths      map[string]map[string]json.RawMessage `json:"paths"`
	Components struct {
		SecuritySchemes map[string]struct {
			Type         string `json:"type"`
			Scheme       string `json:"scheme"`
			BearerFormat string `json:"bearerFormat"`
		} `json:"securitySchemes"`
	} `json:"components"`
}

// operation is the part of an operation object this test reads. Security is
// a pointer so "no operation-level security" and "an empty one" stay
// distinguishable.
type operation struct {
	OperationID string                 `json:"operationId"`
	Security    *[]map[string][]string `json:"security"`
}

// servedDocument fetches the document over HTTP from a server built the
// ordinary way, so the test reads what a client would read - not the file on
// disk, and not the embedded bytes directly.
func servedDocument(t *testing.T) openAPIDocument {
	t.Helper()
	srv := platformhttp.New(discardLogger(), ":0", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/openapi.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/openapi.json = %d, want %d", rec.Code, http.StatusOK)
	}
	var doc openAPIDocument
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("served document does not parse: %v", err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.1.") {
		t.Fatalf("openapi = %q, want a 3.1.x document", doc.OpenAPI)
	}
	return doc
}

// documentedOperations returns the document's operations keyed the way a Go
// ServeMux pattern reads them: "METHOD /path".
func documentedOperations(t *testing.T, doc openAPIDocument) map[string]operation {
	t.Helper()
	ops := make(map[string]operation)
	for path, item := range doc.Paths {
		for method, raw := range item {
			if !slices.Contains(httpMethods, method) {
				continue
			}
			var op operation
			if err := json.Unmarshal(raw, &op); err != nil {
				t.Fatalf("%s %s does not parse as an operation: %v", method, path, err)
			}
			ops[strings.ToUpper(method)+" "+path] = op
		}
	}
	if len(ops) == 0 {
		t.Fatal("the document describes no operations")
	}
	return ops
}

// registeredPatterns is every route this binary mounts: the platform's own
// plus each module's, reported by the same maps the routers are built from.
func registeredPatterns() []string {
	return slices.Concat(platformhttp.Patterns(), content.Patterns(), editorial.Patterns())
}

func TestOpenAPIDocumentDescribesEveryRegisteredRoute(t *testing.T) {
	t.Parallel()
	documented := documentedOperations(t, servedDocument(t))

	for _, pattern := range registeredPatterns() {
		if _, ok := documented[pattern]; !ok {
			t.Errorf("the router registers %q, which the OpenAPI document does not describe; add it to api/openapi.json", pattern)
		}
	}
}

func TestOpenAPIDocumentDescribesNothingUnserved(t *testing.T) {
	t.Parallel()
	documented := documentedOperations(t, servedDocument(t))
	registered := registeredPatterns()

	for pattern := range documented {
		if slices.Contains(registered, pattern) {
			continue
		}
		t.Errorf("the OpenAPI document describes %q, which no route serves; implement it or drop it from api/openapi.json", pattern)
	}
}

// TestOpenAPISecurityMatchesTheAuthGate pins the other half of the contract
// the document makes: the bearer scheme covers the editorial endpoints and
// only those. Everything else is public, and a reader endpoint that started
// demanding a token - or an editorial one that stopped - would be a silent
// change to who may call what.
func TestOpenAPISecurityMatchesTheAuthGate(t *testing.T) {
	t.Parallel()
	doc := servedDocument(t)

	scheme, ok := doc.Components.SecuritySchemes["bearerAuth"]
	if !ok {
		t.Fatal("the document defines no bearerAuth security scheme")
	}
	if scheme.Type != "http" || scheme.Scheme != "bearer" || scheme.BearerFormat != "JWT" {
		t.Errorf("bearerAuth = %+v, want an http bearer scheme carrying JWTs", scheme)
	}
	// A root-level empty requirement is what makes every operation without
	// its own security public rather than merely unstated.
	if doc.Security == nil || len(*doc.Security) != 0 {
		t.Errorf("root security = %v, want an empty list so unmarked operations are public", doc.Security)
	}

	for pattern, op := range documentedOperations(t, doc) {
		_, path, _ := strings.Cut(pattern, " ")
		editorialOp := strings.HasPrefix(path, editorialPrefix)
		switch {
		case editorialOp && !requiresBearer(op):
			t.Errorf("%s is behind the editor gate but the document does not require bearerAuth on it", pattern)
		case !editorialOp && op.Security != nil:
			t.Errorf("%s needs no token, but the document gives it operation-level security %v", pattern, *op.Security)
		}
	}
}

// requiresBearer reports whether an operation demands the bearer scheme.
func requiresBearer(op operation) bool {
	if op.Security == nil {
		return false
	}
	for _, requirement := range *op.Security {
		if _, ok := requirement["bearerAuth"]; ok {
			return true
		}
	}
	return false
}

// TestReaderPatternsAreReachable proves the content module's list is not
// bookkeeping that drifted from its mux. The module answers anything under
// /api/v1 it did not route with "no such endpoint", so a pattern that lost
// its registration has to be caught by the detail rather than the status
// code - the catch-all would otherwise dress the loss up as an ordinary 404.
//
// Each probe is refused before the handler reaches the database, which is
// what makes a nil handle safe here.
func TestReaderPatternsAreReachable(t *testing.T) {
	t.Parallel()
	h := content.NewHandler(discardLogger(), nil)

	probes := map[string]string{
		// No lang, so the handler itself refuses it; the catch-all would
		// answer this path with a 405 instead.
		"GET /api/v1/front": "/api/v1/front",
		// An id that is not a UUID: refused as "no such article" before any
		// query, where an unrouted path would be "no such endpoint".
		"GET /api/v1/articles/{id}": "/api/v1/articles/not-a-uuid",
	}

	for _, pattern := range content.Patterns() {
		t.Run(pattern, func(t *testing.T) {
			t.Parallel()
			method, _, ok := strings.Cut(pattern, " ")
			if !ok {
				t.Fatalf("pattern %q is not %q", pattern, "METHOD /path")
			}
			path, ok := probes[pattern]
			if !ok {
				t.Fatalf("no probe for %q; add one so the pattern is proved reachable", pattern)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))

			var problem struct {
				Detail string `json:"detail"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
				t.Fatalf("answer to %s %s is not problem+json: %v", method, path, err)
			}
			if strings.Contains(problem.Detail, "no such endpoint") {
				t.Fatalf("%s is listed by Patterns() but the catch-all answered it: %q", pattern, problem.Detail)
			}
		})
	}
}

// TestTheDocumentSurvivesTheReaderCatchAll pins a collision the router
// resolves silently: the content module is mounted on the whole /api/v1/
// namespace and answers everything under it that it did not route, while the
// document lives at /api/v1/openapi.json. The more specific pattern wins -
// but that is a property of ServeMux, not a decision recorded anywhere, so
// it is asserted against a server wired the way the composition root wires
// it.
func TestTheDocumentSurvivesTheReaderCatchAll(t *testing.T) {
	t.Parallel()
	srv := platformhttp.New(discardLogger(), ":0", nil)
	srv.Mount(readerPrefix, content.NewHandler(discardLogger(), nil))

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/openapi.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/openapi.json behind the reader catch-all = %d, want %d (body %q)",
			rec.Code, http.StatusOK, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json - the catch-all answered instead", ct)
	}
}

// TestEditorialPatternsAreReachable proves the editorial list is not
// bookkeeping either: every pattern is answered by a real handler, and every
// one of them sits under the prefix the composition root mounts. The module
// answers anything under its prefix it did not route in problem+json - so,
// like the reader probe above, a pattern that lost its registration has to
// be caught by the catch-all's detail rather than by a status code the
// catch-all would dress up as an ordinary answer.
func TestEditorialPatternsAreReachable(t *testing.T) {
	t.Parallel()
	h := editorial.NewHandler(discardLogger(), unreachableStore{}, alwaysEditor{})

	for _, pattern := range editorial.Patterns() {
		t.Run(pattern, func(t *testing.T) {
			t.Parallel()
			method, path, ok := strings.Cut(pattern, " ")
			if !ok {
				t.Fatalf("pattern %q is not %q", pattern, "METHOD /path")
			}
			if !strings.HasPrefix(path, editorialPrefix) {
				t.Fatalf("%q is outside the mounted prefix %q, so nothing would reach it", pattern, editorialPrefix)
			}
			req := httptest.NewRequest(method, path, strings.NewReader(""))
			req.Header.Set("Authorization", "Bearer probe")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			// The empty body or store failure is answered by the handler
			// itself; a pattern nobody registered falls to the catch-all,
			// whose two answers are named below.
			var problem struct {
				Detail string `json:"detail"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
				t.Fatalf("answer to %s %s is not problem+json: %v (body %q)", method, path, err, rec.Body.String())
			}
			if strings.Contains(problem.Detail, "no such endpoint") ||
				strings.Contains(problem.Detail, "is not allowed on this endpoint") {
				t.Fatalf("%s is listed by Patterns() but the catch-all answered it: %q", pattern, problem.Detail)
			}
		})
	}
}

// alwaysEditor authenticates every token, so the probe reaches the routes
// rather than the gate in front of them.
type alwaysEditor struct{}

func (alwaysEditor) AuthenticateEditor(context.Context, string) (editorial.Editor, error) {
	return editorial.Editor{ID: uuid.New(), Email: "probe@example.test", DisplayName: "Route Probe"}, nil
}

// unreachableStore fails loudly: the probe sends bodies no handler accepts,
// so persistence must never be reached.
type unreachableStore struct{}

func (unreachableStore) CreateSource(context.Context, editorial.NewSource) (editorial.Source, error) {
	return editorial.Source{}, errors.New("the route probe must not reach the store")
}

func (unreachableStore) ListSources(context.Context, editorial.SourcesQuery) (editorial.SourcesPage, error) {
	return editorial.SourcesPage{}, errors.New("the route probe must not reach the store")
}

func (unreachableStore) LastPollCycle(context.Context) (editorial.PollCycle, error) {
	return editorial.PollCycle{}, errors.New("the route probe must not reach the store")
}

func (unreachableStore) ReviewQueue(context.Context, editorial.QueueQuery) (editorial.QueuePage, error) {
	return editorial.QueuePage{}, errors.New("the route probe must not reach the store")
}

func (unreachableStore) Approve(context.Context, editorial.NewApproval) (editorial.Article, error) {
	return editorial.Article{}, errors.New("the route probe must not reach the store")
}

func (unreachableStore) Publish(context.Context, uuid.UUID, uuid.UUID) (editorial.Article, error) {
	return editorial.Article{}, errors.New("the route probe must not reach the store")
}

func (unreachableStore) Withdraw(context.Context, uuid.UUID, uuid.UUID, string) (editorial.Withdrawal, error) {
	return editorial.Withdrawal{}, errors.New("the route probe must not reach the store")
}

func (unreachableStore) Provenance(context.Context, uuid.UUID) (editorial.Provenance, error) {
	return editorial.Provenance{}, errors.New("the route probe must not reach the store")
}
