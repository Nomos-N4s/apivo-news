// Where a member may be paid (T086, FR-051): [Destination] and the ownership
// rule every read of one is shaped by.
//
// One file, because a destination is only ever interesting in the company of
// the account it belongs to, and separating the two would be separating a
// value from the only thing that makes it safe to use.

package payout

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout/store"
)

// The sentinels a destination is refused with.
var (
	// ErrNoDestinationStore reports a [Destinations] built over no database
	// handle. It is a wiring mistake rather than a member's, and it is
	// refused rather than panicking in a request.
	ErrNoDestinationStore = errors.New("payout: no database handle to read destinations with")
	// ErrDestinationNotFound reports a destination this member does not
	// have - whether because it does not exist or because it is somebody
	// else's. The two are deliberately one error.
	//
	// The contract answers 403 when a destination does not belong to the
	// caller, and answering the same for an id that names nothing is what
	// stops the endpoint from confirming another member's destination id is
	// real. A caller that could tell the cases apart would eventually
	// report the difference.
	ErrDestinationNotFound = errors.New("payout: no such destination for this member")
	// ErrInvalidDestination reports a destination that could not be
	// recorded: no member, a kind the column does not know, or nothing
	// saying where the details live.
	ErrInvalidDestination = errors.New("payout: the request does not describe a destination")
)

// Kind is how a member is paid, in the vocabulary
// payout_destination_kind_known accepts. It is a closed set with no valid
// zero value: a destination whose kind nobody stated is one no rail could
// pick up.
type Kind string

const (
	// KindSEPA is a bank transfer in the single euro payments area.
	KindSEPA Kind = "sepa"
	// KindManual is a destination an operator settles by hand, recording
	// the reference afterwards. Still holds C-4 and C-5: money leaves once,
	// and only with a named approver.
	KindManual Kind = "manual"
	// KindStub is the rail that moves no money, for exercising the
	// withdrawal path end to end without a bank.
	KindStub Kind = "stub"
)

// Valid reports whether k is one of the three kinds the column accepts. The
// zero value is not.
func (k Kind) Valid() bool {
	switch k {
	case KindSEPA, KindManual, KindStub:
		return true
	default:
		return false
	}
}

// String renders the kind for errors, logs and test failures.
func (k Kind) String() string { return string(k) }

// ParseKind turns a caller's word into a [Kind], refusing anything outside
// the closed set with an error wrapping [ErrInvalidDestination].
func ParseKind(s string) (Kind, error) {
	kind := Kind(s)
	if !kind.Valid() {
		return "", fmt.Errorf("%w: %s is not a payout kind; it is one of %q, %q or %q",
			ErrInvalidDestination, strconv.Quote(s), KindSEPA, KindManual, KindStub)
	}
	return kind, nil
}

// Destination is where a member may be paid.
//
// It carries no bank details and it never will. DetailsRef names where they
// live; the money schema should not be the thing that leaks an IBAN, and
// neither should this package's logs, errors or test failures.
type Destination struct {
	// ID is the destination a withdrawal request names.
	ID uuid.UUID
	// AccountID is the member it belongs to. Carried on the value because
	// every use of a destination is a use on behalf of somebody, and a
	// value that had to be paired up again later is one that eventually
	// is not.
	AccountID uuid.UUID
	// Kind is how the money moves.
	Kind Kind
	// DetailsRef is a reference into the store that holds the actual
	// details. Never the details.
	DetailsRef string
	// VerifiedAt is when the member proved this destination is theirs
	// (FR-051), or the zero value if nobody has. One-way: a verification is
	// never withdrawn or re-dated, so a request that passed the check
	// cannot later be reasoned about as if it had not.
	VerifiedAt time.Time
	// VerifiedMethod is how they proved it. Present exactly when VerifiedAt
	// is - payout_destination_verification_all_or_none makes "verified,
	// method unknown" unstorable, because it is not a verification anyone
	// could defend later.
	VerifiedMethod string
	// CreatedAt is when the destination was recorded.
	CreatedAt time.Time
}

// Verified reports whether anybody has proved this destination belongs to the
// member. A withdrawal against one that has not is refused (FR-051), and the
// check is a method rather than a caller's comparison so there is one answer
// to it.
func (d Destination) Verified() bool { return !d.VerifiedAt.IsZero() }

// NewDestination is what a member asks to record, before it has an id.
type NewDestination struct {
	// AccountID is the member the destination belongs to.
	AccountID uuid.UUID
	// Kind is how the money would move.
	Kind Kind
	// DetailsRef is where the details live. The caller stores them and
	// hands over the reference; raw details must not reach this package.
	DetailsRef string
}

// Validate refuses a destination the table would refuse, wrapping
// [ErrInvalidDestination].
func (n NewDestination) Validate() error {
	if n.AccountID == uuid.Nil {
		return fmt.Errorf("%w: no member owns it", ErrInvalidDestination)
	}
	if !n.Kind.Valid() {
		return fmt.Errorf("%w: %s is not a payout kind", ErrInvalidDestination, strconv.Quote(n.Kind.String()))
	}
	if strings.TrimSpace(n.DetailsRef) == "" {
		// Blank is refused rather than defaulted: the column exists to say
		// WHERE the details are, and a destination answering "nowhere" is
		// one no rail could ever pay.
		return fmt.Errorf("%w: nothing says where the details for this %s destination live",
			ErrInvalidDestination, n.Kind)
	}
	return nil
}

// Destinations reads and records where members may be paid.
//
// Every method takes the account as well as the destination, and that is the
// whole design rather than an inconvenience: ownership travels with the key,
// so there is no call in this package that a later caller could make without
// it.
type Destinations struct {
	q *store.Queries
}

// NewDestinations builds the store over a pool or a transaction.
func NewDestinations(db store.DBTX) (*Destinations, error) {
	if db == nil {
		return nil, ErrNoDestinationStore
	}
	return &Destinations{q: store.New(db)}, nil
}

// Record writes a new destination, unverified.
//
// It comes back unverified because verification is a separate flow (FR-051)
// and because a destination that arrived verified would be one nobody proved
// belongs to the member - which is the whole thing the check exists to stop.
func (d *Destinations) Record(ctx context.Context, n NewDestination) (Destination, error) {
	if err := n.Validate(); err != nil {
		return Destination{}, err
	}
	row, err := d.q.CreatePayoutDestination(ctx, store.CreatePayoutDestinationParams{
		AccountID:  pgtype.UUID{Bytes: n.AccountID, Valid: true},
		Kind:       n.Kind.String(),
		DetailsRef: n.DetailsRef,
	})
	if err != nil {
		// The reference is not repeated. It is not a credential, but it is
		// a key to one, and an error is the least controlled string in a
		// system.
		return Destination{}, fmt.Errorf("payout: recording a %s destination for member %s: %w",
			n.Kind, n.AccountID, err)
	}
	return destinationFrom(row), nil
}

// List answers every destination this member has, oldest first.
func (d *Destinations) List(ctx context.Context, accountID uuid.UUID) ([]Destination, error) {
	if accountID == uuid.Nil {
		return nil, fmt.Errorf("%w: no member to list destinations for", ErrInvalidDestination)
	}
	rows, err := d.q.ListPayoutDestinationsForAccount(ctx, pgtype.UUID{Bytes: accountID, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("payout: listing destinations for member %s: %w", accountID, err)
	}
	destinations := make([]Destination, 0, len(rows))
	for _, row := range rows {
		destinations = append(destinations, destinationFrom(row))
	}
	return destinations, nil
}

// Get answers one destination, if it is this member's.
//
// A destination belonging to somebody else and one that does not exist come
// back as the same [ErrDestinationNotFound], and the account is part of the
// query rather than a comparison afterwards. Both halves matter: the first
// stops the endpoint confirming another member's id is real, and the second
// means there is no call a later caller could make without the check.
func (d *Destinations) Get(ctx context.Context, accountID, id uuid.UUID) (Destination, error) {
	if accountID == uuid.Nil || id == uuid.Nil {
		return Destination{}, fmt.Errorf("%w: a destination is read for a member, by id", ErrInvalidDestination)
	}
	row, err := d.q.GetPayoutDestinationForAccount(ctx, store.GetPayoutDestinationForAccountParams{
		ID:        pgtype.UUID{Bytes: id, Valid: true},
		AccountID: pgtype.UUID{Bytes: accountID, Valid: true},
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Destination{}, fmt.Errorf("%w: %s", ErrDestinationNotFound, id)
	case err != nil:
		return Destination{}, fmt.Errorf("payout: reading destination %s for member %s: %w", id, accountID, err)
	}
	return destinationFrom(row), nil
}

// id and accountID render the value's identifiers back in the shape the
// generated store takes, so a caller inside this package never rebuilds a
// pgtype by hand and never passes the two in the wrong order.
func (d Destination) id() pgtype.UUID { return pgtype.UUID{Bytes: d.ID, Valid: true} }

func (d Destination) accountID() pgtype.UUID { return pgtype.UUID{Bytes: d.AccountID, Valid: true} }

// pgtypeText spells a present string the way the generated store takes one.
func pgtypeText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

// destinationFrom turns a row into the value, spelling an unverified
// destination as the zero time rather than as a null the caller has to
// remember to check.
func destinationFrom(row store.CashbackPayoutDestination) Destination {
	return Destination{
		ID:             uuid.UUID(row.ID.Bytes),
		AccountID:      uuid.UUID(row.AccountID.Bytes),
		Kind:           Kind(row.Kind),
		DetailsRef:     row.DetailsRef,
		VerifiedAt:     row.VerifiedAt.Time,
		VerifiedMethod: row.VerifiedMethod.String,
		CreatedAt:      row.CreatedAt.Time,
	}
}
