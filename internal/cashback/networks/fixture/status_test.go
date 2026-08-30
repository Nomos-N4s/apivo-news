// The tests for status.go: contract rule 2's totality, held against this
// network's own vocabulary in both directions - every word it is known to say
// maps, and a word it is not says so rather than defaulting.

package fixture

import (
	"errors"
	"strings"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

func TestMapTransactionStatusCoversEveryRecordedWord(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw  string
		want networks.Status
	}{
		{raw: "pending", want: networks.StatusPending},
		{raw: "approved", want: networks.StatusConfirmed},
		{raw: "declined", want: networks.StatusDeclined},
		{raw: "void", want: networks.StatusReversed},
	}

	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			t.Parallel()
			got, err := mapTransactionStatus("FIX-1", tc.raw)
			if err != nil {
				t.Fatalf("mapTransactionStatus(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("mapTransactionStatus(%q) = %s, want %s", tc.raw, got, tc.want)
			}
		})
	}
}

// TestMapTransactionStatusRefusesAWordNobodyMapped is contract rule 2. The
// two available guesses are each wrong in a way nobody would notice - call it
// pending and a member's money is withheld with nothing logged, call it
// confirmed and money the network never approved is paid out - so the only
// safe answer is the one an operator has to look at.
func TestMapTransactionStatusRefusesAWordNobodyMapped(t *testing.T) {
	t.Parallel()

	got, err := mapTransactionStatus("FIX-9001", "held_for_review")
	if !errors.Is(err, networks.ErrUnmappableStatus) {
		t.Fatalf("mapTransactionStatus(\"held_for_review\") error = %v, want one wrapping ErrUnmappableStatus", err)
	}
	if got != "" {
		t.Errorf("mapTransactionStatus returned status %q beside its refusal; a refused mapping must carry no status at all", got)
	}
	for _, want := range []string{"FIX-9001", "held_for_review", "pending"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v; an operator has to decide what the new word means, and needs the transaction and what this network was known to say", want, err)
		}
	}
}

func TestMapMerchantStatusCoversEveryRecordedWord(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw  string
		want networks.MerchantStatus
	}{
		{raw: "live", want: networks.MerchantStatusActive},
		{raw: "suspended", want: networks.MerchantStatusPaused},
		{raw: "gone", want: networks.MerchantStatusLeftNetwork},
	}

	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			t.Parallel()
			got, err := mapMerchantStatus("FIXM-1", tc.raw)
			if err != nil {
				t.Fatalf("mapMerchantStatus(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("mapMerchantStatus(%q) = %s, want %s", tc.raw, got, tc.want)
			}
		})
	}
}

// TestMapMerchantStatusRefusesAWordNobodyMapped holds the catalogue half of
// rule 2. A retailer who has left the network is exactly what a catalogue
// poll exists to discover, so a word defaulted to active goes on publishing
// an offer on a route that can no longer pay.
func TestMapMerchantStatusRefusesAWordNobodyMapped(t *testing.T) {
	t.Parallel()

	if _, err := mapMerchantStatus("FIXM-99", "under_review"); !errors.Is(err, networks.ErrUnmappableStatus) {
		t.Fatalf("mapMerchantStatus(\"under_review\") error = %v, want one wrapping ErrUnmappableStatus", err)
	}
}

// TestMappingTablesTranslateRatherThanEcho refuses a table whose every entry
// is the identity. The bug these tables exist to catch is a network word
// silently read as a domain word; a table where the two are the same string
// would compile, pass every case above, and catch nothing.
func TestMappingTablesTranslateRatherThanEcho(t *testing.T) {
	t.Parallel()

	translated := 0
	for raw, status := range transactionStatuses {
		if raw != string(status) {
			translated++
		}
	}
	if translated == 0 {
		t.Errorf("every transaction status word maps to itself, so the table proves nothing about translation")
	}
	translated = 0
	for raw, status := range merchantStatuses {
		if raw != string(status) {
			translated++
		}
	}
	if translated == 0 {
		t.Errorf("every merchant status word maps to itself, so the table proves nothing about translation")
	}
}

// TestKnownWordsIsStable holds the refusal message steady between runs. Two
// identical failures that print their words in different orders read as two
// different failures, and an operator comparing yesterday's alert with
// today's would think the network had changed again.
func TestKnownWordsIsStable(t *testing.T) {
	t.Parallel()

	const want = `"approved", "declined", "pending", "void"`
	for range 8 {
		if got := knownWords(transactionStatuses); got != want {
			t.Fatalf("knownWords(transactionStatuses) = %s, want %s", got, want)
		}
	}
	if got := knownWords(merchantStatuses); got != `"gone", "live", "suspended"` {
		t.Errorf("knownWords(merchantStatuses) = %s", got)
	}
}
