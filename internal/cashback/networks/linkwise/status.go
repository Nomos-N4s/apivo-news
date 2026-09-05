// Linkwise's own status vocabulary and the table that maps it into the
// domain's (T247, FR-033).
//
// The vocabulary is established from two places that disagree in casing and
// agree in substance: the usage text a 400 returns names the words the status
// FILTER accepts - "pending", "validated", "cancelled" and
// "pending_validated" - and the recording in testdata/ shows what the report
// RETURNS, which is capitalised and nested under an object. An adapter that
// compared against the filter's spelling would map every transaction to
// unknown, so the lookup is done on a case-folded word and both spellings
// arrive at the same entry.

package linkwise

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// transactionStatuses maps Linkwise's words into the domain state machine.
//
// A map rather than a switch, for the reason the fixture gives for its own:
// contract rule 2 forbids a default branch, and a map lookup has no way to
// grow one by accident. A switch acquires a default the first time somebody
// wants the compiler to stop complaining, and the cheapest default - call it
// pending - withholds a member's money with nothing logged anywhere.
//
// THREE ENTRIES, NOT FOUR, AND THAT IS THE FINDING. The comment below this
// table says what is missing and why it is not invented here.
//
// "pending_validated" is deliberately absent although the usage text lists
// it. It is a FILTER value meaning "pending OR validated" - two states, asked
// for together - not a state a transaction is in. Mapping it would assign one
// domain status to a word that means two, which is exactly the silent
// mis-crediting rule 2 exists to prevent. If it ever arrives as a returned
// status it meets [networks.ErrUnmappableStatus] and an operator looks at it,
// which is the correct outcome.
var transactionStatuses = map[string]networks.Status{
	"pending":   networks.StatusPending,
	"validated": networks.StatusConfirmed,
	"cancelled": networks.StatusDeclined,
}

// LINKWISE CANNOT DISTINGUISH A DECLINE FROM A REVERSAL, and so neither can
// this table. The domain separates the two because they are different events
// about money: declined is a transaction the advertiser never validated, and
// reversed is money the network reported, validated, and later took back -
// which can outrun a payout, and is therefore absorbed by a house account
// rather than clawed back from the member.
//
// Linkwise reports "Cancelled" for both. There is one status field, it
// carries one of three words, and the word is the same whether the
// transaction was refused on day one or taken back on day sixty. So
// [networks.StatusReversed] is UNREACHABLE on this network, and this is a
// property of the network rather than a hole in the table: adding a fourth
// entry would mean inventing a Linkwise word that does not exist.
//
// What that costs, precisely: a reversal arrives here as a decline. Both
// answer "the network owes nothing" (internal/cashback/ops.CurrentReport.owed
// treats them identically), so reconciliation is unaffected - but anything
// that treats a reversal as its own event sees a decline instead.
//
// Where the distinction can still be recovered: the supersede chain. A report
// arriving as declined whose CURRENT stored row is confirmed is a reversal by
// definition, and that is knowledge the ingestion path has and an adapter
// deliberately does not - adapters translate one report at a time and hold no
// state (contract rule 6). That is where it belongs if it is needed.
//
// One candidate signal is NOT being read, on purpose. Every recorded row
// carries "amended": "No", and an amended transaction is plausibly what
// Linkwise calls one whose terms changed after the fact. Plausibly is not
// evidence: no recorded row carries "Yes", so nothing here can be tested
// against a real one, and a mapping written from a guess about money is worse
// than a documented gap. The field survives regardless - every report carries
// its whole row verbatim in RawPayload - so the day somebody sees a real
// reversal, the evidence to write the rule from is already stored.

// mapTransactionStatus translates one reported status word, refusing an
// unrecognised one with an error wrapping [networks.ErrUnmappableStatus]
// (contract rule 2).
//
// The word is case-folded first: the filter spells it "validated" and the
// report answers "Validated", and an adapter that matched only one of those
// would map every real transaction to unknown while passing every test
// written from the documentation.
//
// The refusal names the transaction and lists what this network is known to
// say, because the operator who receives it has to decide what the new word
// means before anybody's money moves.
func mapTransactionStatus(externalID, raw string) (networks.Status, error) {
	status, mapped := transactionStatuses[strings.ToLower(strings.TrimSpace(raw))]
	if !mapped {
		return "", fmt.Errorf("%w: transaction %s carries %s, and this network is known to report %s",
			networks.ErrUnmappableStatus, strconv.Quote(externalID), strconv.Quote(raw),
			knownWords(transactionStatuses))
	}
	return status, nil
}

// knownWords renders the mapping table's keys in a stable order, so the
// refusal above can say what the network was known to report without a
// message that reorders itself between runs and makes two identical failures
// look like two different ones.
func knownWords(table map[string]networks.Status) string {
	words := slices.Sorted(maps.Keys(table))
	for i, word := range words {
		words[i] = strconv.Quote(word)
	}
	return strings.Join(words, ", ")
}
