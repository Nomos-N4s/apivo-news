// The entry as this package reads it, and the conversions at the database's
// edge (T069).

package earnings

import (
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// Entry is money owed to a member for one purchase, as it stands now.
type Entry struct {
	// ID is the entry, and what a transition names.
	ID uuid.UUID
	// Member is who is owed, and Brand is the tenant they are owed by
	// (ADR-0004): an entry is money owed to a member BY A BRAND, and neither
	// is derivable from the other.
	Member uuid.UUID
	Brand  string
	// Report is the evidence this credit rests on (C-2), and Click the click
	// it was attributed through. Click is the zero uuid only where an
	// operator attributed the purchase by hand from the unattributed queue,
	// because the network named no reference for a guard to read (FR-034).
	Report uuid.UUID
	Click  uuid.UUID
	// State is where it sits now, and HoldRule the rule holding it when that
	// state is held. The two move together: the schema refuses a held entry
	// with no rule and an unheld one carrying a rule.
	State    State
	HoldRule string
	// Amount is what the member is owed. Always positive: a reversal is a
	// separate entry citing the one it undoes, never a negative amount
	// (SC-010).
	Amount money.Amount
	// ReversalOf names the entry this one undoes, and is the zero uuid on an
	// entry that is not a reversal.
	ReversalOf uuid.UUID
}

// entryFrom turns one stored row into the value a caller reads.
//
// The amount is built through [money.New] rather than assembled, because a
// row is only ever as good as the currency beside it and this is the last
// place that can be checked before somebody decides money on it (C-6).
func entryFrom(row store.CashbackEntry) (Entry, error) {
	amount, err := money.New(row.AmountMinor, money.Currency(row.Currency))
	if err != nil {
		return Entry{}, fmt.Errorf("earnings: entry %v: %w", row.ID, err)
	}
	return Entry{
		ID:         uuid.UUID(row.ID.Bytes),
		Member:     uuid.UUID(row.AccountID.Bytes),
		Brand:      row.BrandID,
		Report:     uuid.UUID(row.NetworkTransactionID.Bytes),
		Click:      uuid.UUID(row.ClickID.Bytes),
		State:      State(row.State),
		HoldRule:   row.HoldRule.String,
		Amount:     amount,
		ReversalOf: uuid.UUID(row.ReversalOfID.Bytes),
	}, nil
}

// pgUUID carries a uuid the caller has already decided is present.
func pgUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

// pgUUIDOrNull writes the zero uuid as SQL null.
//
// The zero uuid is not a member, an operator or an entry; storing it would
// put a foreign key violation where a null belongs, and on a nullable column
// it would read as "somebody" rather than "nobody".
func pgUUIDOrNull(id uuid.UUID) pgtype.UUID {
	if id == uuid.Nil {
		return pgtype.UUID{}
	}
	return pgUUID(id)
}

// pgTextOrNull writes the empty string as SQL null, because every text column
// this package writes is checked not-blank when present: an empty reason or
// an empty hold rule is a refusal, and absence is what was meant.
func pgTextOrNull(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}
