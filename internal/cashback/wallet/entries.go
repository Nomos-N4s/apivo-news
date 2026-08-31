// A member's own history: what they earned, where, and what became of it
// (T079, US3).
//
// The wallet's totals say how much; this says what the money is made of. A
// member who cannot see the purchases behind a balance cannot check it, and
// a balance nobody can check is a number they are asked to trust.
//
// Reversals are entries like any other and are listed like any other. A
// reversing entry cites the credit it undoes (SC-010), so both halves of a
// clawback appear, in order, with the reason the money went back - which is
// the one thing a member will want to read and the one a summary would hide.

package wallet

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// Paging bounds, from the contract: limit defaults to 20 and never exceeds
// 100. Bounded here as well as at the boundary, because a caller inside the
// process is a caller too.
const (
	// DefaultPageSize is the page a caller that named no limit asks for.
	DefaultPageSize = 20
	// MaxPageSize is the largest page this endpoint will build, whatever
	// was asked for. A member's history is unbounded and a page that could
	// be too is a query nobody has budgeted for.
	MaxPageSize = 100
)

var (
	// ErrNoEntryReader reports a history built with nothing to read from.
	ErrNoEntryReader = errors.New("wallet: a history needs somewhere to read the entries from")
	// ErrNoMemberToList reports a page asked for nobody.
	ErrNoMemberToList = errors.New("wallet: a history needs the member it is about")
	// ErrUnknownState reports a filter naming a state no entry can be in.
	// Refused rather than answered with an empty page: an empty page reads
	// as "you have earned nothing", and a member told that because of a
	// typo would believe it.
	ErrUnknownState = errors.New("wallet: no entry is ever in that state")
	// ErrBadCursor reports a cursor this endpoint did not issue.
	ErrBadCursor = errors.New("wallet: malformed cursor")
	// ErrNotListed reports a page that could not be read.
	ErrNotListed = errors.New("wallet: the member's entries could not be read")
)

// States is every state an entry can be in, and the whole set a filter may
// name (FR-042). Spelled out here rather than derived from the earnings
// module, because this module may not import it - earnings reads this one.
var States = []string{"held", "pending", "confirmed", "reserved", "paid", "reversed"}

// MerchantName is a retailer's name and the language it is in.
//
// The language travels with the name because the name alone cannot say
// whether it is the one the member reads. A deployment shows a merchant's
// own-language name when it has no copy in the member's language, and US5
// scenario 2 says that must be LABELLED rather than passed off - which a
// client can only do if it is told.
type MerchantName struct {
	// Name is the retailer's name, or "" for an entry with no click to
	// reach a retailer through - one an operator attributed by hand.
	Name string
	// Language is the BCP-47 primary subtag Name is in, or "" when there
	// is no name.
	Language string
	// Asked reports whether Language is the one the caller asked for. False
	// with a Name set means this is a fallback and must be shown as one.
	Asked bool
}

// Entry is one line of a member's history.
type Entry struct {
	ID uuid.UUID
	// State is where this entry sits now (FR-042).
	State string
	// Amount is the member's share, and Sale what they spent. Two amounts
	// and not one, because a member checks a credit by recognising the
	// purchase behind it.
	Amount money.Amount
	Sale   money.Amount
	// TransactedAt is when the purchase happened, as the network reported
	// it, and CreatedAt when this entry was written. The first is what a
	// member recognises; the second is what the list is ordered by, because
	// only it is stable under a network that reports out of order.
	TransactedAt time.Time
	CreatedAt    time.Time
	// Merchant is where the purchase was made.
	Merchant MerchantName
	// HoldRule names the rule holding this entry back, and is empty unless
	// the state is held. It is shown where the totals do not show held
	// money at all: a member seeing an entry they cannot count is owed the
	// reason.
	HoldRule string
	// ReversalOf names the entry this one undoes, and is the zero uuid on
	// an entry that undoes nothing.
	ReversalOf uuid.UUID
	// Reason is why the entry exists, recorded with the transition that
	// opened it. On a reversal it is why the money went back.
	Reason string
}

// Page is one page of history and the cursor for the next.
type Page struct {
	Entries []Entry
	// NextCursor is the opaque position to ask for the next page with, or
	// "" when this page is the last. Empty rather than a cursor that would
	// return nothing, so a client stops by reading the answer rather than
	// by making one more request to discover it.
	NextCursor string
}

// PageRequest is one ask for a page of history.
type PageRequest struct {
	// Member is whose history it is. Taken from the token by the handler
	// and never from the request.
	Member uuid.UUID
	// State filters to one state, or lists every state when empty.
	State string
	// Language is the BCP-47 primary subtag to name merchants in. Empty
	// asks for none, and every name then comes back as a fallback.
	Language string
	// Limit is the page size asked for; zero means DefaultPageSize and
	// anything above MaxPageSize is capped rather than refused.
	Limit int
	// Cursor is where to continue from, or "" for the first page.
	Cursor string
}

// EntryReader is the read this needs. Named here per the boundary rules;
// *store.Queries satisfies it.
type EntryReader interface {
	MemberEntries(ctx context.Context, arg store.MemberEntriesParams) ([]store.MemberEntriesRow, error)
}

// History lists a member's own entries.
type History struct {
	entries EntryReader
}

// NewHistory builds the reader, refusing a nil one.
func NewHistory(entries EntryReader) (*History, error) {
	if entries == nil {
		return nil, ErrNoEntryReader
	}
	return &History{entries: entries}, nil
}

// Page answers one page of the member's history.
//
// One row more than the page is read, and the extra is not returned. That is
// what lets the answer say whether there is a next page without a second
// query and without a count: a count over an unbounded history is work that
// grows with the member, and a client that had to ask for an empty page to
// learn it was finished would make one request per member per session for
// nothing.
func (h *History) Page(ctx context.Context, req PageRequest) (Page, error) {
	if req.Member == uuid.Nil {
		return Page{}, ErrNoMemberToList
	}
	if req.State != "" && !knownState(req.State) {
		return Page{}, fmt.Errorf("%w: %q", ErrUnknownState, req.State)
	}
	at, id, err := decodeCursor(req.Cursor)
	if err != nil {
		return Page{}, err
	}

	size := req.Limit
	switch {
	case size <= 0:
		size = DefaultPageSize
	case size > MaxPageSize:
		size = MaxPageSize
	}

	rows, err := h.entries.MemberEntries(ctx, store.MemberEntriesParams{
		Language:  req.Language,
		AccountID: pgtype.UUID{Bytes: req.Member, Valid: true},
		State:     textOrNull(req.State),
		CursorAt:  at,
		CursorID:  id,
		PageSize:  int32(size) + 1,
	})
	if err != nil {
		return Page{}, fmt.Errorf("%w: %s: %w", ErrNotListed, req.Member, err)
	}

	page := Page{Entries: make([]Entry, 0, min(len(rows), size))}
	for _, row := range rows[:min(len(rows), size)] {
		entry, err := entryFrom(row, req.Language)
		if err != nil {
			return Page{}, err
		}
		page.Entries = append(page.Entries, entry)
	}
	if len(rows) > size {
		last := page.Entries[len(page.Entries)-1]
		page.NextCursor = encodeCursor(last.CreatedAt, last.ID)
	}
	return page, nil
}

// entryFrom turns one stored row into the value a member is shown.
//
// Both amounts go through [money.New] rather than being assembled, because a
// row is only ever as good as the currency beside it and this is the last
// place either can be checked before somebody is shown it (C-6).
func entryFrom(row store.MemberEntriesRow, language string) (Entry, error) {
	amount, err := money.New(row.AmountMinor, money.Currency(row.Currency))
	if err != nil {
		return Entry{}, fmt.Errorf("%w: entry %v: %w", ErrNotListed, row.ID, err)
	}
	sale, err := money.New(row.SaleAmountMinor, money.Currency(row.SaleCurrency))
	if err != nil {
		return Entry{}, fmt.Errorf("%w: entry %v: the purchase behind it: %w", ErrNotListed, row.ID, err)
	}
	return Entry{
		ID:           uuid.UUID(row.ID.Bytes),
		State:        row.State,
		Amount:       amount,
		Sale:         sale,
		TransactedAt: row.TransactedAt.Time,
		CreatedAt:    row.CreatedAt.Time,
		Merchant:     merchantFrom(row, language),
		HoldRule:     row.HoldRule.String,
		ReversalOf:   uuid.UUID(row.ReversalOfID.Bytes),
		Reason:       row.Reason.String,
	}, nil
}

// merchantFrom picks the name to show and says which language it is in.
//
// The language asked for wins; the merchant's own is the fallback, and it is
// returned marked as one. Neither exists for an entry with no click, and
// that is not a defect: an operator attributed it by hand because the
// network named no reference, so there is no route to a retailer to follow.
func merchantFrom(row store.MemberEntriesRow, language string) MerchantName {
	if row.NameInLanguageAsked.Valid {
		return MerchantName{Name: row.NameInLanguageAsked.String, Language: language, Asked: true}
	}
	if row.NameInMerchantsLanguage.Valid {
		return MerchantName{Name: row.NameInMerchantsLanguage.String, Language: row.SourceLanguageCode.String}
	}
	return MerchantName{}
}

// knownState reports whether s is a state an entry can be in.
func knownState(s string) bool {
	for _, known := range States {
		if s == known {
			return true
		}
	}
	return false
}

// textOrNull writes the empty string as SQL null, which the query reads as
// "every state" rather than as a state named "".
func textOrNull(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}
