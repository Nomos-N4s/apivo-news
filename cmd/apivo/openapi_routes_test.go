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

	"github.com/Nomos-N4s/apivo-news/internal/editorial"
	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// documentedButNotServedHere lists operations the document describes that a
// binary built from this tree does not serve. There is exactly one reason
// for an entry: the reader endpoints (internal/content/http.go) are merged
// but not yet forward-integrated into main, while the frontend already
// consumes them - so the document describes the implementation rather than
// pretending they do not exist.
//
// Each entry is asserted to be genuinely absent below, so the day the
// implementation arrives this test fails and the entry has to go. The list
// can only shrink, and nothing can be parked in it to dodge the comparison.
var documentedButNotServedHere = []string{
	"GET /api/v1/front",
	"GET /api/v1/articles/{id}",
}

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
	return slices.Concat(platformhttp.Patterns(), editorial.Patterns())
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
		if slices.Contains(registered, pattern) || slices.Contains(documentedButNotServedHere, pattern) {
			continue
		}
		t.Errorf("the OpenAPI document describes %q, which no route serves; implement it or drop it from api/openapi.json", pattern)
	}

	for _, pattern := range documentedButNotServedHere {
		if _, ok := documented[pattern]; !ok {
			t.Errorf("documentedButNotServedHere names %q, which the document does not describe; drop the stale entry", pattern)
		}
		if slices.Contains(registered, pattern) {
			t.Errorf("%q is served now, so remove it from documentedButNotServedHere - the document and the router agree about it", pattern)
		}
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

// TestRegisteredPatternsAreReachable proves the pattern lists are not
// bookkeeping that drifted from the muxes: every editorial pattern is
// answered by a real handler (the platform's own are probed in its package),
// and every one of them sits under the prefix the composition root mounts.
func TestRegisteredPatternsAreReachable(t *testing.T) {
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
			// The empty body is rejected by the handler itself; a pattern
			// nobody registered would be answered by the mux with a 404.
			if rec.Code == http.StatusNotFound {
				t.Fatalf("%s is listed by Patterns() but the router does not serve it", pattern)
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
