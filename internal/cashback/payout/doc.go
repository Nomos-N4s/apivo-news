// Package payout is the money leaving: where a member may be paid, the
// withdrawal that asks for it, and the rails that carry it.
//
// It is the one place in the cashback domain where value leaves the business,
// so the two invariants that matter most in this repository are both its:
// C-4, that no money moves without a named human approver, and C-5, that a
// submission carried out twice moves money once.
//
// It never holds bank details. cashback.payout_destination stores a
// REFERENCE to them and nothing else, and nothing in this package sees an
// account number - losing the money database must not be losing anybody's
// IBAN, and an error message is the least controlled string in a system.
package payout
