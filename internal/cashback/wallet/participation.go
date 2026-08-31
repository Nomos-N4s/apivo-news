// A member's opt-in as this module reads it (T080, FR-001..003).
//
// The row and the value are kept apart for one reason: the row's currency
// is three characters of char(3) and its two instants are nullable, and
// every one of those is a shape the rest of the module must not have to
// think about. What comes out here is a currency that has been checked
// (C-6), and a leaving date that is either a date or the zero Time - never
// "a timestamp that says nothing until you look at a boolean beside it".

package wallet

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// The two statuses the schema allows (participation_status_known). A member
// is in cashback or they have left it; there is no third state, and no
// "pending" - accepting the terms IS the opt-in, so there is nothing to
// wait for between the two.
const (
	// StatusActive is a member currently in cashback.
	StatusActive = "active"
	// StatusLeft is a member who has left (FR-003). Their financial record
	// is untouched: entries continue to resolve, payouts already made stand,
	// and the row is closed rather than deleted.
	StatusLeft = "left"
)

// ErrNotParticipation reports a stored row this module cannot make a
// participation out of. It is a failure of the database's shape rather than
// of the request, so it reaches a caller as a 500.
var ErrNotParticipation = errors.New("wallet: the stored participation could not be read")

// Participation is one member's opt-in.
type Participation struct {
	// Member is whose opt-in this is.
	Member uuid.UUID
	// Brand is the brand they accepted terms from (ADR-0004). Frozen by the
	// schema once written, so this is the brand at the time they joined and
	// not necessarily the one the process is running as today.
	Brand string
	// OptedInAt is when they accepted, re-stated on each re-join.
	OptedInAt time.Time
	// TermsVersion is which version of the terms they accepted (FR-002).
	TermsVersion string
	// Status is StatusActive or StatusLeft.
	Status string
	// LeftAt is when they left, and is the zero Time on an active
	// participation. The schema makes the two agree
	// (participation_left_has_timestamp), so a caller may read either one
	// and never both.
	LeftAt time.Time
	// Currency is the currency their wallet is denominated in.
	//
	// It is the BRAND's default (ADR-0004), recorded at the moment they
	// accepted, and it is not what GET /wallet answers in: that is the
	// withdrawal threshold's currency, because the threshold is the figure
	// a balance is compared against (see Wallets.Of). The two are the same
	// value in any coherent deployment and nothing here enforces that,
	// because neither key is the other's authority - a brand's default is
	// what a member gets before they choose, and the threshold is what a
	// withdrawal is checked against.
	Currency money.Currency
}

// Active reports whether this member is currently in cashback.
func (p Participation) Active() bool { return p.Status == StatusActive }

// participationFrom converts one stored row.
//
// The currency goes through money.ParseCurrency rather than being copied
// across, for the reason every amount in this module goes through money.New:
// this is the last place a value can be checked before somebody is shown it
// beside a figure, and char(3) will hold three characters of anything.
func participationFrom(row store.CashbackParticipation) (Participation, error) {
	currency, err := money.ParseCurrency(row.DefaultCurrency)
	if err != nil {
		return Participation{}, fmt.Errorf("%w: member %x's default currency: %w",
			ErrNotParticipation, row.AccountID.Bytes, err)
	}
	if row.Status != StatusActive && row.Status != StatusLeft {
		return Participation{}, fmt.Errorf("%w: status %q is neither %q nor %q",
			ErrNotParticipation, row.Status, StatusActive, StatusLeft)
	}
	held := Participation{
		Member:       uuid.UUID(row.AccountID.Bytes),
		Brand:        row.BrandID,
		OptedInAt:    row.OptedInAt.Time,
		TermsVersion: row.TermsVersion,
		Status:       row.Status,
		Currency:     currency,
	}
	if row.LeftAt.Valid {
		held.LeftAt = row.LeftAt.Time
	}
	return held, nil
}
