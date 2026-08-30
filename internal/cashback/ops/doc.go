// Package ops serves the cashback operator surface: the queues a human has
// to work and the decisions only a human may take (FR-060, FR-061).
//
// Everything here is gated on the operator role, which migration 0019 made
// expressible and payout_insert_guard makes binding. The HTTP gate is the
// polite early answer - a 403 with a reason instead of a constraint
// violation from deep inside a write - and never the guarantee; the
// database says the same thing again on every row that matters.
//
// Two rules shape the whole package. Every action records who took it and
// why, in the same transaction as the effect it had, because an audit
// record written afterwards is a record of what somebody remembered
// (FR-061). And nothing here edits or deletes: a queue row's resolution is
// appended to it, a mistaken resolution is corrected by a new fact in the
// stream, and the evidence a decision was taken against stays exactly as
// it was read (C-3).
package ops
