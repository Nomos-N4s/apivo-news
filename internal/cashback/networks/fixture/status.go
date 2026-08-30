// The fixture network's own status vocabulary and the two tables that map it
// into the domain's, with the refusal a word nobody mapped meets. One file,
// because contract rule 2's totality is one rule and both tables are held to
// it in the same way.

package fixture

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// transactionStatuses maps the words the recorded responses use for a
// transaction into the domain state machine (FR-033).
//
// Two of the four are deliberately not the domain's own words. A mapping
// table whose every entry was the identity would compile, pass, and prove
// nothing: the bug this table exists to catch is a network word silently
// read as a domain word, and a fixture that made the two the same could not
// catch it. So "approved" is what the network calls a validated transaction -
// which is the reference network's word too (ADR-0003) - and "void" is what
// it calls one it has taken back, and both have to be translated to be
// understood.
//
// The table is a map rather than a switch for one reason: contract rule 2
// forbids a default branch, and a map lookup has no way to grow one by
// accident. A switch acquires a default the first time somebody wants the
// compiler to stop complaining, and the cheapest default - call it pending -
// withholds a member's money with nothing logged anywhere.
var transactionStatuses = map[string]networks.Status{
	"pending":  networks.StatusPending,
	"approved": networks.StatusConfirmed,
	"declined": networks.StatusDeclined,
	"void":     networks.StatusReversed,
}

// merchantStatuses maps the words the recorded catalogue uses for a route
// into the domain's three (migration 0011). None of the three is a domain
// word, for the reason above, and the recording carries one of each so a
// catalogue import has all three to handle.
//
// Rule 2's totality is not only about transactions. A retailer who has left
// the network is precisely what a catalogue poll exists to discover, and a
// word nobody mapped is the same silence there as it is on a transaction:
// default it to active and the offer goes on being published on a route that
// can no longer pay.
var merchantStatuses = map[string]networks.MerchantStatus{
	"live":      networks.MerchantStatusActive,
	"suspended": networks.MerchantStatusPaused,
	"gone":      networks.MerchantStatusLeftNetwork,
}

// mapTransactionStatus translates one recorded status word, refusing an
// unrecognised one with an error wrapping [networks.ErrUnmappableStatus]
// (contract rule 2).
//
// The refusal names the transaction and lists what this network is known to
// say, because the operator who receives it has to decide what the new word
// means before anybody's money moves, and "unknown status" on its own sends
// them to the network's documentation with no idea what changed.
func mapTransactionStatus(externalID, raw string) (networks.Status, error) {
	status, mapped := transactionStatuses[raw]
	if !mapped {
		return "", fmt.Errorf("%w: transaction %s carries %s, and this network is known to report %s",
			networks.ErrUnmappableStatus, strconv.Quote(externalID), strconv.Quote(raw),
			knownWords(transactionStatuses))
	}
	return status, nil
}

// mapMerchantStatus translates one recorded route status word, refusing an
// unrecognised one the same way and with the same sentinel, so one operator
// alert covers a network that invented a word about a transaction and one
// that invented a word about a retailer.
func mapMerchantStatus(externalID, raw string) (networks.MerchantStatus, error) {
	status, mapped := merchantStatuses[raw]
	if !mapped {
		return "", fmt.Errorf("%w: merchant %s carries %s, and this network is known to report %s",
			networks.ErrUnmappableStatus, strconv.Quote(externalID), strconv.Quote(raw),
			knownWords(merchantStatuses))
	}
	return status, nil
}

// knownWords renders a mapping table's keys in a stable order, so the two
// refusals above can say what the network was known to report without a
// message that reorders itself between runs and makes two identical failures
// look like two different ones.
func knownWords[V ~string](table map[string]V) string {
	words := slices.Sorted(maps.Keys(table))
	for i, word := range words {
		words[i] = strconv.Quote(word)
	}
	return strings.Join(words, ", ")
}
