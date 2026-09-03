// Package scenarios is the quickstart's acceptance gates, run as tests
// (T129).
//
// [quickstart.md] states, for each of V1 to V6, what to run and what must be
// true afterwards, and says "every one has an automated equivalent in the
// suite". This is that suite, and the name of each case is the NAME the
// Makefile takes:
//
//	make cashback-scenario NAME=earn-confirm
//
// # What this layer is, and what it is not
//
// It is not more unit coverage. Every module beneath it already tests its
// own behaviour in depth, and duplicating that here would be two places to
// keep in step. What this asserts is the sentence in the quickstart -
// nothing more, nothing invented - so that a founder reading "wallet
// Pending, then Confirmed, each equal to an independently computed ledger
// sum" can run one command and be told whether that is true today.
//
// It follows that when a scenario fails, the module beneath it is where the
// fault is. This package holds no logic of its own to be wrong.
//
// # Why one transaction, rolled back
//
// Each scenario runs inside a transaction against the database DATABASE_URL
// names, and rolls it back. Not a throwaway database: the point of an
// acceptance gate is the real schema, with its real constraints, and a
// rollback leaves the founder's own database untouched afterwards. The
// refusals a scenario provokes deliberately - an UPDATE against evidence, a
// second payout - raise inside nested transactions, which pgx runs as
// savepoints, so a refusal aborts the savepoint rather than poisoning the
// scenario around it.
//
// [quickstart.md]: https://github.com/Nomos-N4s/apivo-news/blob/main/specs/002-apivo-cashback-alpha/quickstart.md
package scenarios
