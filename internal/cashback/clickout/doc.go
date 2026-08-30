// Package clickout owns the tracked redirect: the click reference Apivo
// mints, and the click row every later credit rests on (US1, FR-020..023).
//
// One ordering rule shapes the package, and it is FR-020's: the click record
// and its reference exist BEFORE the redirect is issued. A member who is
// redirected first and recorded second buys through a reference no row can
// match, and the transaction comes back attributable to nobody - a failure
// that costs them real money while looking to them exactly like success.
//
// Two things follow from that and are enforced here rather than trusted.
// A reference is minted from cryptographic entropy or not at all: there is
// no fallback source, because a predictable reference is one an attacker can
// claim somebody else's purchase with (FR-020). And a click names its member:
// account_id is NOT NULL in the schema and refused here before the insert,
// because an anonymous click that could later be adopted by an account is
// exactly what FR-023 forbids.
//
// The row is append-only (C-3). What the member was promised at click time -
// the published band and the member's share of it - is snapshotted onto it,
// so a rate changed afterwards never reaches back into a credit already
// earned (FR-013).
package clickout
