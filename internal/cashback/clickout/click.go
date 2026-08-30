// The click itself: what is recorded before a redirect, and how a reported
// reference finds its way back to it (T063, FR-013, FR-020..023).

package clickout

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout/store"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

var (
	// ErrNoClickStore reports a recorder built with nothing to write to.
	ErrNoClickStore = errors.New("clickout: recording clicks needs a store to write to")
	// ErrAnonymousClick reports a click that names no member. FR-023: an
	// anonymous click can never later be credited to an account, and the
	// cheapest way to guarantee that is for it never to exist. The schema
	// says the same thing with a NOT NULL; this says it before the insert,
	// so the caller gets the rule rather than a constraint name.
	ErrAnonymousClick = errors.New("clickout: a click names the member who made it")
	// ErrUnofferedClick reports a click that names no offer. Without one
	// there is no band to snapshot and nothing the credit could be computed
	// from.
	ErrUnofferedClick = errors.New("clickout: a click names the offer it was made on")
	// ErrNotRecorded reports a click that could not be written. FR-020 puts
	// the record before the redirect, so a caller that swallowed this would
	// redirect a member whose purchase can never be matched back.
	ErrNotRecorded = errors.New("clickout: the click could not be recorded")
	// ErrReferenceTaken reports a click reference already issued to another
	// click. At 128 bits this is not bad luck: it is a broken entropy
	// source or a caller re-using a reference, and either way the redirect
	// must not happen.
	ErrReferenceTaken = errors.New("clickout: this click reference is already issued")
	// ErrNoSuchClick reports a reported reference that matches no click.
	//
	// Ordinary, not a failure: a network may echo a reference from another
	// publisher, from a stale link, or from nothing at all. The caller
	// queues that transaction as unattributed (FR-034); what it must never
	// do is widen the match until something is found.
	ErrNoSuchClick = errors.New("clickout: no click carries this reference")
)

// ContextDigest is the privacy-minimised device or context fingerprint a
// click may carry (FR-022): enough for abuse rules to tell two clicks apart,
// and nothing that reconstructs who or where somebody is.
//
// It is a type rather than a string because the column's rule is about what
// must NOT reach it. The field is unexported and [NewContextDigest] is the
// only way to fill it, so a raw address, user agent or device fingerprint
// cannot be stored by a caller that meant to hash it and forgot - which is
// the failure FR-022 names, and the kind that is discovered years later in a
// table nobody thought held personal data.
//
// What it gives and does not give is worth being exact about. The digest is
// unsalted SHA-256, so equal inputs give equal digests: that is what makes
// it useful to an abuse rule and it is also its limit. It hides the input
// from a reader of the table; it does not stop somebody who can GUESS the
// input from confirming it. Nothing here should ever be treated as
// anonymising a value whose space is small enough to enumerate.
//
// The zero value is "no context was digested", which is a legitimate state:
// the column is nullable and a click with nothing to digest records nothing
// rather than an empty string pretending to be a digest.
type ContextDigest struct {
	digest string
}

// contextDigestSeparator joins the parts of a digest's input so that two
// different splittings cannot produce the same bytes - ("ab","c") and
// ("a","bc") are different contexts and must not collide.
const contextDigestSeparator = "\x00"

// NewContextDigest digests the given parts. Parts that are entirely blank
// are dropped, and a call with nothing left returns the zero value: there is
// a difference between a context nobody recorded and a context recorded as
// empty, and only the first has a representation in the column.
func NewContextDigest(parts ...string) ContextDigest {
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			kept = append(kept, part)
		}
	}
	if len(kept) == 0 {
		return ContextDigest{}
	}
	sum := sha256.Sum256([]byte(strings.Join(kept, contextDigestSeparator)))
	return ContextDigest{digest: hex.EncodeToString(sum[:])}
}

// Recorded reports whether any context was digested at all.
func (d ContextDigest) Recorded() bool { return d.digest != "" }

// String returns the digest, or the empty string when none was recorded.
func (d ContextDigest) String() string { return d.digest }

// Promise is what the member was told at click time, and what governs the
// credit however the published offer changes afterwards (FR-013).
//
// Both halves travel together because both are needed to compute a member's
// share and neither is meaningful alone: the band says what the network
// pays, the share says how much of that is the member's. The schema stores
// them in two columns - one jsonb, one integer - and this is the pair they
// make.
type Promise struct {
	// Rate is the published band, snapshotted whole.
	Rate catalogue.RateBand
	// MemberShare is the member's share of the commission in basis points,
	// as published at that moment.
	MemberShare money.BasisPoints
}

// Validate reports a promise that could not govern a credit.
func (p Promise) Validate() error {
	if _, err := json.Marshal(p.Rate); err != nil {
		return fmt.Errorf("clickout: the promised rate cannot be snapshotted: %w", err)
	}
	if !p.MemberShare.Valid() {
		return fmt.Errorf("clickout: a member share of %d basis points is outside 0..%d",
			p.MemberShare, money.BasisPointsScale)
	}
	return nil
}

// NewClick is the click to record: who, on what, with which reference, and
// what they were promised.
//
// There is no clicked_at here. The row's own clock stamps it, for the reason
// every other instant in this schema is read back rather than supplied: a
// caller's clock is a second answer to when this happened, and this one is
// the moment a member's credit is dated from.
type NewClick struct {
	// Ref is the reference minted for this click, and the value the network
	// will echo back against the purchase.
	Ref networks.IssuedClickRef
	// AccountID is the member. Never the nil uuid (FR-023).
	AccountID uuid.UUID
	// OfferID is the band clicked through.
	OfferID uuid.UUID
	// Promised is the rate and share as published at this moment (FR-013).
	Promised Promise
	// Context is the optional privacy-minimised digest (FR-022).
	Context ContextDigest
}

// Click is what the database recorded, read back rather than echoed.
type Click struct {
	ID        uuid.UUID
	Ref       networks.IssuedClickRef
	AccountID uuid.UUID
	OfferID   uuid.UUID
	// ClickedAt is the instant the row carries.
	ClickedAt time.Time
	Promised  Promise
	Context   ContextDigest
}

// ClickStore is the write and the read this file needs, named here per the
// boundary rules - the consumer names its dependency. *store.Queries
// satisfies it over a pool or a transaction, and the caller keeps the
// transaction boundary: FR-020 puts the click before the redirect, and
// whether that commit carries anything else is the caller's decision.
type ClickStore interface {
	InsertClick(ctx context.Context, arg store.InsertClickParams) (store.CashbackClick, error)
	GetClickByRef(ctx context.Context, clickRef string) (store.CashbackClick, error)
}

// Clicks records clicks and finds them again by the reference a network
// reported. It never updates and never deletes: the row is append-only
// evidence (C-3), and a click that turned out to be wrong is corrected by
// what happens to the entry, never by editing what happened.
type Clicks struct {
	store ClickStore
}

// NewClicks builds the recorder over the given store, refusing a nil one.
func NewClicks(s ClickStore) (*Clicks, error) {
	if s == nil {
		return nil, ErrNoClickStore
	}
	return &Clicks{store: s}, nil
}

// Record writes the click, and answers what the database recorded.
//
// Everything it can refuse, it refuses before the insert: a reference that
// was never minted, a click that names no member (FR-023) or no offer, and a
// promise that could not govern a credit. Those are the caller's mistakes
// and they read better as themselves than as a constraint name - and the
// database checks every one of them again anyway.
func (c *Clicks) Record(ctx context.Context, click NewClick) (Click, error) {
	if err := click.Ref.Validate(); err != nil {
		return Click{}, fmt.Errorf("%w: %w", ErrNotRecorded, err)
	}
	if click.AccountID == uuid.Nil {
		return Click{}, ErrAnonymousClick
	}
	if click.OfferID == uuid.Nil {
		return Click{}, ErrUnofferedClick
	}
	if err := click.Promised.Validate(); err != nil {
		return Click{}, fmt.Errorf("%w: %w", ErrNotRecorded, err)
	}
	snapshot, err := json.Marshal(click.Promised.Rate)
	if err != nil {
		// Unreachable: Validate marshalled the same value a moment ago.
		return Click{}, fmt.Errorf("%w: %w", ErrNotRecorded, err)
	}

	row, err := c.store.InsertClick(ctx, store.InsertClickParams{
		ClickRef:               click.Ref.Ref(),
		AccountID:              pgtype.UUID{Bytes: click.AccountID, Valid: true},
		OfferID:                pgtype.UUID{Bytes: click.OfferID, Valid: true},
		RateSnapshot:           snapshot,
		MemberShareBpsSnapshot: int32(click.Promised.MemberShare),
		ContextDigest:          pgtype.Text{String: click.Context.String(), Valid: click.Context.Recorded()},
	})
	if err != nil {
		if referenceTaken(err) {
			return Click{}, fmt.Errorf("%w: %s", ErrReferenceTaken, click.Ref)
		}
		return Click{}, fmt.Errorf("%w: %s: %w", ErrNotRecorded, click.Ref, err)
	}
	return clickFrom(row)
}

// ByRef answers the click a network's reported reference names.
//
// It takes a [networks.ClickRef] - what a network ECHOED - rather than a
// string, so a caller cannot pass a reference of its own by accident, and so
// the absent case has to be handled rather than becoming a lookup for the
// empty string. A report carrying no reference is not a failed match; it is
// a report that never claimed one, and it never reaches here.
//
// A reference matching nothing reports [ErrNoSuchClick], which is ordinary:
// networks echo references from other publishers and from stale links, and
// the caller queues that transaction as unattributed (FR-034).
func (c *Clicks) ByRef(ctx context.Context, reported networks.ClickRef) (Click, error) {
	ref, present := reported.Ref()
	if !present {
		return Click{}, fmt.Errorf("%w: the report carries no reference to match", ErrNoSuchClick)
	}
	row, err := c.store.GetClickByRef(ctx, ref)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Click{}, fmt.Errorf("%w: %s", ErrNoSuchClick, reported)
	case err != nil:
		return Click{}, fmt.Errorf("clickout: reading the click for %s: %w", reported, err)
	}
	return clickFrom(row)
}

// clickFrom turns one stored row into the value a caller reads.
//
// The reference goes back through [networks.NewIssuedClickRef] rather than
// being wrapped: a row is only ever as good as what is in it, and this is
// where a value written before a constraint existed - or by something other
// than this code - would be caught, instead of being handed on as a
// reference a redirect could be rebuilt from.
func clickFrom(row store.CashbackClick) (Click, error) {
	ref, err := networks.NewIssuedClickRef(row.ClickRef)
	if err != nil {
		return Click{}, fmt.Errorf("clickout: click %v: %w", row.ID, err)
	}
	var band catalogue.RateBand
	if err := json.Unmarshal(row.RateSnapshot, &band); err != nil {
		return Click{}, fmt.Errorf("clickout: click %v: the snapshotted rate is unreadable: %w", row.ID, err)
	}
	share := money.BasisPoints(row.MemberShareBpsSnapshot)
	if !share.Valid() {
		return Click{}, fmt.Errorf("clickout: click %v: a snapshotted member share of %d basis points is outside 0..%d",
			row.ID, share, money.BasisPointsScale)
	}
	return Click{
		ID:        uuid.UUID(row.ID.Bytes),
		Ref:       ref,
		AccountID: uuid.UUID(row.AccountID.Bytes),
		OfferID:   uuid.UUID(row.OfferID.Bytes),
		ClickedAt: row.ClickedAt.Time,
		Promised:  Promise{Rate: band, MemberShare: share},
		// Filled directly rather than through NewContextDigest: this value
		// was digested when the click was recorded, and hashing it again
		// would answer a different question. The constructor guards what
		// goes INTO the column; this is what came out of it.
		Context: ContextDigest{digest: row.ContextDigest.String},
	}, nil
}

// referenceTaken reports the one unique violation this insert can raise, by
// the constraint's name rather than by SQLSTATE alone: 23505 on this table
// could equally be click_id_account_unique, which would mean something else
// entirely.
func referenceTaken(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		pgErr.Code == pgerrcode.UniqueViolation &&
		pgErr.ConstraintName == "click_ref_unique"
}
