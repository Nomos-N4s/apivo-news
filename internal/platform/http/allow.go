package http

import (
	"net/http"
	"slices"
	"strings"
)

// AllowTable answers what methods a known path accepts - the test for
// "wrong method" rather than "wrong address", and the Allow header HTTP
// requires on the 405 that follows.
//
// A module builds one from its own route table, so a route it registers is
// classified here by construction: drift between a mux and its 405
// classifier is unrepresentable rather than merely tested for. It lives in
// the platform because the alternative is one copy per module, and the
// copies would answer differently the moment one of them gained a corner
// the others did not - which is exactly the corner a 405 is about.
type AllowTable []allowEntry

// allowEntry is one registered path pattern, pre-split into segments (a
// "{name}" segment matches any single non-empty path segment) beside the
// Allow header its methods render to.
type allowEntry struct {
	segments []string
	methods  string
}

// NewAllowTable groups "METHOD /path" patterns by path and renders each
// path's Allow header. GET implies HEAD, exactly as ServeMux serves every
// "GET /path" pattern for HEAD requests too. A pattern that is not
// "METHOD /path" is skipped: a bare path registers for every method and so
// never reaches a catch-all to be classified.
func NewAllowTable(patterns []string) AllowTable {
	byPath := make(map[string][]string, len(patterns))
	for _, pattern := range patterns {
		method, path, ok := strings.Cut(pattern, " ")
		if !ok {
			continue
		}
		byPath[path] = append(byPath[path], method)
		if method == http.MethodGet {
			byPath[path] = append(byPath[path], http.MethodHead)
		}
	}
	table := make(AllowTable, 0, len(byPath))
	for path, methods := range byPath {
		slices.Sort(methods)
		table = append(table, allowEntry{
			segments: strings.Split(path, "/"),
			methods:  strings.Join(slices.Compact(methods), ", "),
		})
	}
	return table
}

// MethodsFor reports the Allow header for a path some registered pattern
// matches, or "" for a path this table does not serve.
func (t AllowTable) MethodsFor(path string) string {
	segments := strings.Split(path, "/")
	for _, entry := range t {
		if matchesPattern(entry.segments, segments) {
			return entry.methods
		}
	}
	return ""
}

// matchesPattern reports whether a request path's segments satisfy a
// registered pattern's, segment by segment: a "{name}" wildcard takes any
// single non-empty segment, everything else matches literally. This mirrors
// how ServeMux itself reads the pattern, minus the corners a catch-all
// never sees (ServeMux canonicalises doubled slashes and dots away with a
// redirect before any handler runs).
func matchesPattern(pattern, path []string) bool {
	if len(pattern) != len(path) {
		return false
	}
	for i, want := range pattern {
		wildcard := strings.HasPrefix(want, "{") && strings.HasSuffix(want, "}")
		if wildcard {
			if path[i] == "" {
				return false
			}
			continue
		}
		if want != path[i] {
			return false
		}
	}
	return true
}
