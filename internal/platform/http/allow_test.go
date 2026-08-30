package http_test

// The 405 classifier, tested where it now lives. Its whole job is to tell
// "this address exists, that method does not" apart from "no such address",
// and to render the Allow header the first of those owes the client.

import (
	"testing"

	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// routes is a table with the shapes that matter: a path with two methods, a
// GET-only path, a wildcard path, and a nested wildcard path.
var routes = []string{
	"GET /api/v1/things",
	"POST /api/v1/things",
	"GET /api/v1/things/{id}",
	"DELETE /api/v1/things/{id}",
	"GET /api/v1/things/{id}/history",
}

func TestAllowTableRendersEveryMethodAPathAccepts(t *testing.T) {
	t.Parallel()
	table := platformhttp.NewAllowTable(routes)

	for path, want := range map[string]string{
		// GET implies HEAD, because ServeMux serves every "GET /path"
		// pattern for HEAD too - so a HEAD that fell through to the
		// catch-all would be told HEAD is not allowed on a path that
		// serves it.
		"/api/v1/things":                 "GET, HEAD, POST",
		"/api/v1/things/abc":             "DELETE, GET, HEAD",
		"/api/v1/things/abc/history":     "GET, HEAD",
		"/api/v1/things/0198-a-uuid-ish": "DELETE, GET, HEAD",
	} {
		if got := table.MethodsFor(path); got != want {
			t.Errorf("MethodsFor(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestAllowTableKnowsNothingOfPathsNobodyRegistered(t *testing.T) {
	t.Parallel()
	table := platformhttp.NewAllowTable(routes)

	// Each of these is a near miss, because a near miss is what a hand-
	// written classifier gets wrong: one segment too few, one too many, an
	// empty segment where a wildcard wants a value, and a literal that
	// differs.
	for _, path := range []string{
		"/api/v1",
		"/api/v1/things/abc/history/deep",
		"/api/v1/things/",
		"/api/v1/thingz",
		"/api/v2/things",
	} {
		if got := table.MethodsFor(path); got != "" {
			t.Errorf("MethodsFor(%q) = %q, want %q - this path is not registered", path, got, "")
		}
	}
}

// TestAllowTableIgnoresAPatternWithNoMethod pins the one input the table
// deliberately drops. A bare path registers for every method, so ServeMux
// answers it whatever the verb and the catch-all is never reached; listing
// it here would render an Allow header for a 405 that cannot happen.
func TestAllowTableIgnoresAPatternWithNoMethod(t *testing.T) {
	t.Parallel()
	table := platformhttp.NewAllowTable([]string{"/api/v1/anything"})

	if got := table.MethodsFor("/api/v1/anything"); got != "" {
		t.Errorf("MethodsFor(%q) = %q, want %q", "/api/v1/anything", got, "")
	}
}

// TestAllowTableDeduplicatesTheImpliedHead covers the corner where a module
// registers HEAD itself beside GET: the Allow header must name it once.
func TestAllowTableDeduplicatesTheImpliedHead(t *testing.T) {
	t.Parallel()
	table := platformhttp.NewAllowTable([]string{"GET /api/v1/thing", "HEAD /api/v1/thing"})

	const want = "GET, HEAD"
	if got := table.MethodsFor("/api/v1/thing"); got != want {
		t.Errorf("MethodsFor(%q) = %q, want %q", "/api/v1/thing", got, want)
	}
}
