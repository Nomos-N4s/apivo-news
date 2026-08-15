// Package api holds the OpenAPI 3.1 description of the HTTP surface and
// embeds it into the binary.
//
// The package exists only because go:embed cannot reach across directories:
// the document belongs at the conventional api/openapi.json, so the code
// that embeds it has to live beside it. It has no dependencies and no
// behaviour, which is what lets the platform HTTP server - the bottom layer,
// forbidden from importing any module - serve the document.
package api

import _ "embed"

//go:embed openapi.json
var openAPIJSON []byte

// OpenAPIJSON returns the committed api/openapi.json, byte for byte: the
// same document CI validates. Callers must not modify the returned slice -
// it is the embedded copy, not a clone, because it is written to a response
// on every request and never edited.
func OpenAPIJSON() []byte { return openAPIJSON }
