// What a network reported, carried verbatim and judged afterwards: [Reported]
// for a transaction, [ReportedMerchant] for a catalogue entry, and the
// payload and country checks both are held to. One file, because the two
// share every rule about evidence and the shared checks have exactly two
// callers.

package networks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// The sentinels a report is refused with before any caller sees it. Each
// names a way the evidence row could not have been written, so a report that
// would fail at the last INSERT of a window fails here instead.
var (
	// ErrMissingExternalID reports a report that does not carry the
	// network's own identifier for the transaction. That identifier is half
	// of what makes a re-report the same transaction rather than a new one
	// (migration 0012, network_transaction_one_root), so a report without it
	// cannot be superseded, deduplicated or looked up - it is not evidence,
	// it is an anecdote.
	ErrMissingExternalID = errors.New("networks: report carries no external transaction id")
	// ErrMissingStatusRaw reports a report with no verbatim status. The
	// network's own word is kept beside the normalised status so that a
	// mapping bug is provable from the evidence rather than argued from
	// memory (FR-032), and a report that dropped it has already destroyed
	// the only proof that the mapping was right.
	ErrMissingStatusRaw = errors.New("networks: report carries no verbatim status")
	// ErrMissingRawPayload reports a report with no verbatim payload
	// (contract rule 1, FR-032): empty, whitespace, the JSON literal null, or
	// a document carrying no members at all. Each of them satisfies "a
	// payload is present" while carrying nothing a normalisation fix could
	// ever be re-derived from, which is the absence this rule exists to
	// forbid wearing the costume of a payload. An adapter returning one fails
	// its conformance test. The payload is the only thing a normalisation fix
	// can be re-derived from, and a network that has stopped serving a window
	// cannot supply it a second time.
	ErrMissingRawPayload = errors.New("networks: report carries no raw payload")
	// ErrMalformedRawPayload reports a payload the jsonb column will refuse.
	// It is distinct from [ErrMissingRawPayload] because the two are
	// different adapter bugs: one forgot to capture the fragment, the other
	// captured bytes that cannot be stored, and refusing here means the whole
	// window fails visibly at translation rather than at the last INSERT of a
	// batch that had already looked fine.
	//
	// Three refusals, and they are the three Postgres actually makes on bytes
	// that encoding/json is happy with: text that is not JSON at all, bytes
	// that are not UTF-8 ("invalid byte sequence for encoding UTF8"), and a
	// \u0000 or lone-surrogate escape ("unsupported Unicode escape
	// sequence"). Mis-encoded merchant names are ordinary in affiliate
	// payloads, so these are the realistic failure rather than an exotic one.
	// It is not a proof of storability - only the INSERT proves that - it is
	// the set of refusals that are cheap and certain before one.
	ErrMalformedRawPayload = errors.New("networks: raw payload is not storable JSON")
	// ErrMissingTransactedAt reports a report with no transaction time. It is
	// a NOT NULL column (migration 0012) because it is what the confirmation
	// window, the trailing re-read and every member-facing "purchase date"
	// are measured from; a zero time here would date every such answer to the
	// year 1 rather than fail.
	ErrMissingTransactedAt = errors.New("networks: report carries no transaction time")
	// ErrMissingMerchantName reports a catalogue entry with no name. A
	// retailer nobody can name cannot be published, and the alternative to
	// refusing is a catalogue row shown to members as an empty string.
	ErrMissingMerchantName = errors.New("networks: catalogue entry carries no merchant name")
	// ErrInvalidMerchantCountry reports a country that is not two uppercase
	// letters. It mirrors merchant_country_alpha2_format: membership of the
	// real ISO 3166-1 list is reference data rather than a constraint, but a
	// value the column will refuse is worth catching before a whole catalogue
	// import fails on one row.
	ErrInvalidMerchantCountry = errors.New("networks: merchant country is not an ISO 3166-1 alpha-2 code")
)

// Reported is one transaction exactly as a network reported it: the
// normalised facts plus the verbatim payload they were normalised from. It
// is the value an adapter yields and the evidence row is written from
// (migration 0012, cashback.network_transaction).
//
// It carries what the NETWORK said and nothing else. The moment of retrieval
// and the query window asked for are stored beside it in the same row and are
// Apivo's own knowledge rather than the network's, so they are the poller's
// to supply, and an adapter that cannot invent them cannot get them wrong.
// The publisher account is Apivo's knowledge too and is likewise absent from
// here - but the poller does not have to guess it: the adapter states it once
// on the port, at [Network.Account], and the poller reads
// network_account_id from there. Neither the network id nor a content digest
// appears either: the first the caller already knows, since it chose which
// adapter to call, and the second is computed by the database from the
// reported facts, so that a fingerprint can never disagree with the evidence
// it fingerprints.
//
// Nothing here is a judgement. The amounts are carried at whatever sign the
// network stated - a correction really can be negative, and this port's job
// is to record what was said rather than to decide it was impossible - and a
// report with no click reference is ordinary rather than broken (FR-034).
//
// The fields are exported and the struct can be written invalid, unlike
// [ClickRef] and [PublisherAccount] beside it. That is a deliberate limit
// rather than an oversight: an adapter decodes a payload field by field, and
// a constructor taking eight arguments in a fixed order is a worse defence
// against a mis-mapped one than a named field is. [Reported.Validate] is what
// closes it, and contract rule 7 is what obliges every adapter to call it.
type Reported struct {
	// ExternalID is the network's own identifier for the transaction, and
	// the value a later report of the same transaction is recognised by
	// (network_transaction_one_root). Required.
	ExternalID string
	// ClickRef is the attribution reference the network echoed back, or the
	// definite absence of one - never a blank string standing in for
	// absence. See [ClickRef] for why the schema will not let those two
	// collapse, and why it is not the same type as the reference a redirect
	// is built with.
	ClickRef ClickRef
	// StatusRaw is the network's own status word, verbatim and untranslated.
	// It is stored beside the normalised status so a mapping bug is provable
	// from the evidence rather than argued from memory (FR-032). Required,
	// including for a status that mapped cleanly: the evidence is what makes
	// the mapping auditable, so it is not optional on the happy path.
	StatusRaw string
	// Status is StatusRaw mapped into the domain state machine (FR-033).
	// The mapping is total: an adapter that recognises no entry for a word
	// returns [ErrUnmappableStatus] rather than a value from this set, so a
	// Reported that exists at all carries a status somebody mapped
	// deliberately.
	Status Status
	// SaleAmount is what the member spent, in minor units of an explicit
	// currency (C-6). It is evidence rather than an input to any credit -
	// nothing is computed from it - and it is what a member recognises
	// their own purchase by when they read the wallet.
	SaleAmount money.Amount
	// Commission is what the network says it will pay for that sale, in
	// minor units of the same currency. THIS is the figure a member's share
	// is computed from, together with the click-time rate snapshot (FR-040)
	// - never the rate published today, and never a share of the sale.
	Commission money.Amount
	// TransactedAt is when the purchase happened, as the network reports it.
	// Required: it is what the confirmation window and every member-facing
	// purchase date are measured from.
	TransactedAt time.Time
	// RawPayload is the verbatim network response fragment for this
	// transaction (FR-032, contract rule 1). Required, never empty, and
	// never rewritten: normalisation can be wrong, and this is what a fix is
	// re-derived from when the network will no longer serve the window it
	// came from.
	RawPayload json.RawMessage
}

// Validate refuses a report that could not have come from a network,
// wrapping the sentinel that names the mistake. Contract rule 7 requires
// every adapter to call it on every value before yielding it, so a
// translation bug is caught at the adapter that made it rather than at an
// INSERT halfway through a window, and the conformance suite (T051) holds
// every adapter to that. Nothing in this file can call it for them: the port
// declares an interface, and the values crossing it are built inside the
// adapter.
//
// The rules, in the order they are checked:
//
//   - the network's own transaction id is present ([ErrMissingExternalID]);
//   - the click reference is absent or non-blank, never blank
//     ([ErrBlankClickRef]);
//   - the verbatim status is present ([ErrMissingStatusRaw]) and the
//     normalised status is one of the four domain states
//     ([ErrUnmappableStatus], contract rule 2);
//   - both amounts carry a well-formed currency (wrapping
//     [money.ErrInvalidCurrency]) and carry the SAME currency (wrapping
//     [money.ErrCurrencyMismatch]) - the evidence row stores one currency
//     column for both figures, so a report denominating them differently is
//     a report that cannot be stored without one of the two being silently
//     restated. It is contract rule 7 rather than this file's own invention;
//     a network that really reports a sale in the advertiser's currency and
//     a commission in the publisher's has no representation in this schema
//     at all, and finding that out at the port is better than finding it out
//     one restated figure at a time;
//   - the transaction time is set ([ErrMissingTransactedAt]);
//   - the raw payload is present ([ErrMissingRawPayload], contract rule 1)
//     and storable ([ErrMalformedRawPayload]).
//
// The first hole found is the one reported, and the message carries the
// offending value, so a refused report says which of its parts to look at.
//
// What Validate deliberately does not check is anything only the domain
// knows: whether the click reference matches a click, whether the
// transaction was already reported, whether the status may follow the one
// already stored. Those are the ingestion path's questions, answered against
// the database, and none of them can be asked of a value in isolation.
func (r Reported) Validate() error {
	if strings.TrimSpace(r.ExternalID) == "" {
		return fmt.Errorf("%w: %s", ErrMissingExternalID, strconv.Quote(r.ExternalID))
	}
	if err := r.ClickRef.Validate(); err != nil {
		return fmt.Errorf("networks: report %s: %w", strconv.Quote(r.ExternalID), err)
	}
	if strings.TrimSpace(r.StatusRaw) == "" {
		return fmt.Errorf("%w: report %s", ErrMissingStatusRaw, strconv.Quote(r.ExternalID))
	}
	if !r.Status.Valid() {
		return fmt.Errorf("%w: report %s carries %s, normalised from %s",
			ErrUnmappableStatus, strconv.Quote(r.ExternalID),
			strconv.Quote(string(r.Status)), strconv.Quote(r.StatusRaw))
	}
	if err := r.SaleAmount.Validate(); err != nil {
		return fmt.Errorf("networks: report %s: sale amount: %w", strconv.Quote(r.ExternalID), err)
	}
	if err := r.Commission.Validate(); err != nil {
		return fmt.Errorf("networks: report %s: commission: %w", strconv.Quote(r.ExternalID), err)
	}
	if r.SaleAmount.Currency != r.Commission.Currency {
		return fmt.Errorf("%w: report %s reports a sale of %s and a commission of %s, and the evidence row stores one currency for both",
			money.ErrCurrencyMismatch, strconv.Quote(r.ExternalID), r.SaleAmount, r.Commission)
	}
	if r.TransactedAt.IsZero() {
		return fmt.Errorf("%w: report %s", ErrMissingTransactedAt, strconv.Quote(r.ExternalID))
	}
	return validateReportedPayload(r.RawPayload, "report "+strconv.Quote(r.ExternalID))
}

// ReportedMerchant is one catalogue entry exactly as a network reported it:
// the retailer, the route's state, and the verbatim payload it was
// normalised from (FR-012). It is what FetchCatalogue yields and what the
// merchant and merchant_network rows are built from (migration 0011).
//
// Like [Reported] it carries only what the network said. Which brand
// publishes the route, whether it is the preferred one of several, and the
// language the copy is supplied in are all Apivo's decisions rather than the
// network's - the last of them because a network states no BCP-47 subtag,
// and inventing one from an account's region is exactly the kind of guess
// constitution VII exists to prevent. All three are supplied by the import,
// not by this port.
type ReportedMerchant struct {
	// ExternalID is the network's own identifier for the retailer, unique
	// within the network (merchant_network_unique_per_network). It is what
	// an imported catalogue row and a reported transaction are matched back
	// on, so it is required.
	ExternalID string
	// Name is the retailer's name as the network supplies it, which becomes
	// the merchant's copy in its source language. Required: a retailer
	// nobody can name cannot be published, and the alternative to refusing
	// is an empty string shown to members.
	Name string
	// Country is the ISO 3166-1 alpha-2 country the retailer trades in, or
	// empty for a retailer bound to no single country - which the schema
	// spells as a null and this port spells as the zero value, since an
	// unbound merchant is ordinary and needs no separate presence flag the
	// way a click reference does.
	Country string
	// StatusRaw is the network's own word for the route's state, verbatim,
	// kept for the same reason a transaction's is (FR-032). Required.
	StatusRaw string
	// Status is StatusRaw mapped into the domain's closed set. Total, like
	// the transaction mapping: a retailer who has left the network is
	// exactly what a catalogue poll exists to discover, and no adapter may
	// default it.
	Status MerchantStatus
	// RawPayload is the verbatim catalogue payload for this retailer
	// (FR-012). Required, for the same reason a transaction's is: a
	// normalisation fix is re-derived from it rather than re-fetched.
	RawPayload json.RawMessage
}

// Validate refuses a catalogue entry that could not have come from a
// network, wrapping the sentinel that names the mistake. Contract rule 7
// requires every adapter to call it on every value before yielding it.
//
// The rules, in the order they are checked:
//
//   - the network's own merchant id is present ([ErrMissingExternalID]);
//   - the retailer has a name ([ErrMissingMerchantName]);
//   - the country is empty or two uppercase letters
//     ([ErrInvalidMerchantCountry]);
//   - the verbatim status is present ([ErrMissingStatusRaw]) and the
//     normalised status is one of the three route states
//     ([ErrUnmappableStatus], contract rule 2);
//   - the raw payload is present ([ErrMissingRawPayload]) and storable
//     ([ErrMalformedRawPayload]).
func (m ReportedMerchant) Validate() error {
	if strings.TrimSpace(m.ExternalID) == "" {
		return fmt.Errorf("%w: %s", ErrMissingExternalID, strconv.Quote(m.ExternalID))
	}
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("%w: merchant %s", ErrMissingMerchantName, strconv.Quote(m.ExternalID))
	}
	if err := validateReportedCountry(m.Country); err != nil {
		return fmt.Errorf("networks: merchant %s: %w", strconv.Quote(m.ExternalID), err)
	}
	if strings.TrimSpace(m.StatusRaw) == "" {
		return fmt.Errorf("%w: merchant %s", ErrMissingStatusRaw, strconv.Quote(m.ExternalID))
	}
	if !m.Status.Valid() {
		return fmt.Errorf("%w: merchant %s carries %s, normalised from %s",
			ErrUnmappableStatus, strconv.Quote(m.ExternalID),
			strconv.Quote(string(m.Status)), strconv.Quote(m.StatusRaw))
	}
	return validateReportedPayload(m.RawPayload, "merchant "+strconv.Quote(m.ExternalID))
}

// validateReportedPayload holds a raw payload to contract rule 1, for a
// transaction and a catalogue entry alike: present, carrying something, and
// storable in the jsonb column that will hold it.
//
// A JSON null is refused alongside an empty payload rather than accepted as
// valid JSON, and so are the documents that carry no members - {}, [] and any
// bare scalar. jsonb would take every one of them, which is the danger: the
// column would then hold a value satisfying "the payload is present" while
// carrying nothing a normalisation fix could ever be re-derived from - the
// absence this rule exists to forbid, wearing the costume of a payload.
//
// Storability is then three checks rather than json.Valid alone, because
// json.Valid and jsonb disagree on two classes of bytes that affiliate
// payloads produce routinely: a mis-encoded merchant name is not UTF-8, and a
// \u0000 or lone-surrogate escape is JSON that Postgres cannot convert to
// text. Both pass json.Valid and both fail at the INSERT - which is exactly
// the batch-fails-at-its-last-row outcome this refusal exists to move
// earlier.
func validateReportedPayload(payload json.RawMessage, subject string) error {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || bytes.Equal(trimmed, reportedJSONNull) {
		return fmt.Errorf("%w: %s", ErrMissingRawPayload, subject)
	}
	if !json.Valid(trimmed) {
		return fmt.Errorf("%w: %s is not JSON", ErrMalformedRawPayload, subject)
	}
	if !utf8.Valid(trimmed) {
		return fmt.Errorf("%w: %s carries bytes that are not UTF-8, which the column refuses", ErrMalformedRawPayload, subject)
	}
	if fault := reportedPayloadEscapeFault(trimmed); fault != "" {
		return fmt.Errorf("%w: %s %s, which the column refuses", ErrMalformedRawPayload, subject, fault)
	}
	if !reportedPayloadCarriesFacts(trimmed) {
		return fmt.Errorf("%w: %s carries a payload with nothing in it", ErrMissingRawPayload, subject)
	}
	return nil
}

// reportedPayloadCarriesFacts reports whether a payload is a JSON object or
// array with at least one member. A bare scalar is not a network's response
// fragment for one transaction, and an empty object is the same absence a
// JSON null is. The payload has already passed json.Valid, so its last
// non-space byte closes what its first one opened.
func reportedPayloadCarriesFacts(payload []byte) bool {
	switch payload[0] {
	case '{', '[':
		return len(bytes.TrimSpace(payload[1:len(payload)-1])) > 0
	default:
		return false
	}
}

// reportedPayloadEscapeFault names the Unicode escape jsonb will refuse, or
// returns the empty string when a payload carries none. Postgres rejects
// \u0000 ("unsupported Unicode escape sequence") and any surrogate that is
// not half of a well-formed pair; Go's json.Valid accepts both.
//
// The scan tracks whether it is inside a string, because a backslash means
// nothing anywhere else, and it steps over each escape whole. That second
// part is what keeps the six characters \\u0000 - one literal backslash
// followed by the text u0000, which jsonb stores happily - from being read as
// the escape that would be refused, and what keeps the low half of a
// well-formed pair from being read as a lone one.
func reportedPayloadEscapeFault(payload []byte) string {
	inString := false
	for i := 0; i < len(payload); i++ {
		c := payload[i]
		if !inString {
			if c == '"' {
				inString = true
			}
			continue
		}
		switch c {
		case '"':
			inString = false
		case '\\':
			fault, span := reportedEscapeFault(payload[i:])
			if fault != "" {
				return fault
			}
			// The loop's own i++ advances the first byte of the escape.
			i += span - 1
		}
	}
	return ""
}

// reportedEscapeFault judges the escape beginning at rest[0], which is a
// backslash, naming what jsonb would refuse about it and how many bytes it
// spans so the scan above can step over it whole. The payload has already
// passed json.Valid, so every \u carries four hex digits and no backslash
// stands at the very end.
func reportedEscapeFault(rest []byte) (fault string, span int) {
	const (
		unitEscapeLen = 6 // \uXXXX
		shortEscape   = 2 // \" \\ \n and the rest
		highFirst     = 0xD800
		lowFirst      = 0xDC00
		surrogateEnd  = 0xDFFF
	)
	if len(rest) < unitEscapeLen || rest[1] != 'u' {
		return "", min(shortEscape, len(rest))
	}
	unit, err := strconv.ParseUint(string(rest[2:unitEscapeLen]), 16, 32)
	if err != nil {
		return "", shortEscape
	}
	switch {
	case unit == 0:
		return `carries a \u0000 escape`, unitEscapeLen
	case unit < highFirst || unit > surrogateEnd:
		return "", unitEscapeLen
	case unit >= lowFirst:
		return "carries a lone low surrogate escape", unitEscapeLen
	}
	if len(rest) < 2*unitEscapeLen || rest[unitEscapeLen] != '\\' || rest[unitEscapeLen+1] != 'u' {
		return "carries a high surrogate escape with no low half", unitEscapeLen
	}
	low, err := strconv.ParseUint(string(rest[unitEscapeLen+2:2*unitEscapeLen]), 16, 32)
	if err != nil || low < lowFirst || low > surrogateEnd {
		return "carries a high surrogate escape with no low half", unitEscapeLen
	}
	return "", 2 * unitEscapeLen
}

// validateReportedCountry holds a merchant's country to the merchant table's
// own format check: two uppercase ASCII letters, or nothing at all.
// Membership of the real ISO 3166-1 register is deliberately not checked, for
// the reason money gives about ISO 4217 - the register changes, and a copy
// embedded here would go stale and start refusing real countries.
func validateReportedCountry(country string) error {
	if country == "" {
		return nil
	}
	const alpha2Len = 2
	if len(country) != alpha2Len {
		return fmt.Errorf("%w: %s", ErrInvalidMerchantCountry, strconv.Quote(country))
	}
	for i := range alpha2Len {
		if country[i] < 'A' || country[i] > 'Z' {
			return fmt.Errorf("%w: %s", ErrInvalidMerchantCountry, strconv.Quote(country))
		}
	}
	return nil
}
