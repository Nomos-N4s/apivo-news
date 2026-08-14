// Package identity owns JWT validation (Supabase Auth tokens) and
// entitlement checks. All entitlement decisions delegate to the single
// database function is_entitled - logic lives in one place.
//
// Boundary: no other module imports this package's internals. Consumers
// define the interfaces they need and the composition root in cmd wires them.
package identity
