// Package identity owns JWT validation (Supabase Auth tokens) and
// entitlement checks. All entitlement decisions delegate to the single
// database function is_entitled - logic lives in one place.
//
// The package verifies RS256/ES256 tokens against the project's JWKS
// endpoint with cached, auto-refreshing keys (research D4), maps the
// token subject to the account table, and provides the editor gate that
// editorial endpoints apply before the database enforces the same rule
// again on write.
//
// Boundary: no other module imports this package's internals. Consumers
// define the interfaces they need and the composition root in cmd wires them.
// Symmetrically, this package defines narrow interfaces for what it
// consumes: Querier (satisfied by the platform database pool) and
// RoleLookup (the migration-0002 seam documented on that type).
package identity
