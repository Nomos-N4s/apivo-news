// Package networks owns transaction ingestion: what an affiliate network
// reported, when it was retrieved, and the evidence a member's money later
// rests on. Its centrepiece is the [Network] port - the only way any code in
// this repository speaks to an affiliate network (ADR-0003).
//
// The port is consumer-defined. This package declares what the cashback
// domain needs from a network; adapter packages beneath it - one per network,
// plus the fixture adapter that lets the whole ingestion chain be built and
// demonstrated with no publisher account - implement the declaration, and the
// composition root in cmd wires one in. Nothing outside an adapter knows a
// network's vocabulary: every signature below speaks types declared here, in
// platform/money, or the one identifier type the whole repository shares, and
// a network's own status words, field names, pagination scheme and deeplink
// signing stop at its package boundary.
//
// That the port itself drags in no database driver and no generated store is
// held by TestPortDeclarationNamesNoVendor in this package, which reads this
// file's imports. It is a narrow rule and deliberately so: it says that an
// adapter satisfying this port is handed no means of writing a row, which is
// the structural half of contract rule 6. The repository-wide rule - that
// adding a second affiliate network changes only its own adapter (SC-008) -
// is not enforced anywhere yet. It belongs to T109, in
// internal/arch/network_isolation_test.go, written once the package layout it
// has to describe exists; until then SC-008 is an intention this file is
// shaped to keep, not a property the build checks.
//
// Adapters translate, and nothing else (contract rule 6, contracts/ports.md
// section 2). An adapter opens no database transaction, writes no row and
// decides no credit; it turns one network's answer into the values below and
// hands them back. Everything about a retrieval that is Apivo's own knowledge
// rather than the network's - at what moment it was made, for which query
// window - is the poller's to record, which is why none of it appears in
// [Reported] even though the evidence table stores it (migration 0012). The
// one piece of Apivo's knowledge the port does carry is which publisher
// account an adapter polls ([Network.Account]), because the poller cannot
// record a column it has no way to learn. The separation is what keeps a
// mis-translation from being able to move money on its own: a wrong mapping
// produces a wrong row that an operator can see and re-derive, never a credit
// nobody authorised.
//
// Three rules govern every value that crosses this port:
//
//   - The raw payload is required, always (FR-032, contract rule 1).
//     [Reported.RawPayload] is the verbatim fragment the network sent, stored
//     beside the normalised columns, and it is what a normalisation bug is
//     later re-derived from. Networks do not reliably serve their history
//     twice, so a payload not captured at retrieval is a payload gone for
//     good, and with it the ability to fix a credit without the network's
//     cooperation.
//   - Status mapping is total (FR-033, contract rule 2). An unrecognised
//     status word is [ErrUnmappableStatus] and surfaces to an operator. There
//     is no default branch here or in any adapter, because the cheapest wrong
//     answer - "call it pending" - silently withholds a member's money, and
//     the second cheapest - "call it confirmed" - pays out money the network
//     never approved.
//   - Money is an int64 of minor units carrying an explicit ISO-4217 currency
//     (C-6): [money.Amount], and nothing else, in every signature below.
//     There is no float, no decimal string and no implicit currency anywhere
//     in this package.
//
// Two kinds of value cross this port, and they are built differently on
// purpose. What a NETWORK said is carried verbatim and judged afterwards by a
// Validate method, because a port that quietly cleaned a network's answer
// would make the adapter bug that produced it unobservable. What APIVO minted
// - the publisher account an adapter polls, the unguessable click reference a
// redirect carries - is built through a constructor that refuses an unusable
// value, because there is no evidence to preserve in our own mistake and an
// invalid state that cannot be written is better than one that must be
// checked for.
//
// What this package deliberately does not decide is what a report means. A
// [Reported] is evidence of what a network said, not a judgement about it:
// the amounts are carried at whatever sign and size the network stated, an
// unattributed transaction is a perfectly valid report rather than an error
// (FR-034), and turning any of that into a member's balance is the earnings
// and wallet packages' work, from stored rows, after the fact.
//
// This comment lives in its own file rather than beside whichever type was
// written first, which is the convention the repository already follows
// (internal/ingestion/doc.go, internal/translation/doc.go). Every other
// file here then describes only itself: two files each claiming to speak
// for the package is a merge conflict waiting to happen and a godoc page
// that says everything twice.
//
// # Errors
//
// Every failure this package reports wraps one of its sentinel errors, and
// the adapter conformance suite asserts on them with errors.Is, so every
// adapter refuses the same mistake with the same error however differently
// its network expressed it. Match on the sentinel, never on message text.
// Each sentinel is declared beside the rule it enforces rather than pooled
// in one block, so the rule and its refusal are read together.
//
// Failures of the money type itself - a malformed currency, two currencies
// where one was required - wrap that package's own sentinels
// ([money.ErrInvalidCurrency], [money.ErrCurrencyMismatch]) rather than
// duplicates of them: one money representation means one set of money
// errors.
package networks
