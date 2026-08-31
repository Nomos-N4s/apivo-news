// Joining cashback, leaving it, and reading which of the two you did
// (T080, FR-001, FR-002, FR-003).
//
// Every write here opens its own transaction, because the row and its
// event have to commit together: a member who was told they had joined and
// whose acceptance never reached the stream is a consent nobody downstream
// can prove was given. The reads open none.
//
// What this file does NOT do is decide what the terms are. It is handed
// them, once, by the composition root, and refuses to record an acceptance
// of anything else - so "the version in force" has exactly one source in
// the process, and it is the brand definition rather than a constant here.

package wallet

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

var (
	// ErrNoBrand reports a deployment that has not configured a brand
	// definition, so there is no version of the terms to accept and no
	// brand to record the acceptance against (ADR-0004).
	//
	// Refused when a member tries to JOIN rather than when the service is
	// built, and only there. Reading a participation and leaving one need
	// no brand at all - what they touch is already in the row - so a
	// deployment missing its brand file still answers those honestly. Same
	// shape as ErrNoThreshold, and for the same reason: an unmounted
	// endpoint answers 404, which says the API is not here, where this says
	// what is missing and names the key.
	ErrNoBrand = errors.New("wallet: this deployment has no brand definition, so there are no terms to accept")
	// ErrStaleTerms reports an acceptance of a version that is not the one
	// in force. A stale consent is never recorded (FR-002): a member who
	// agreed to last quarter's terms agreed to something the deployment is
	// no longer offering, and writing it down would make the record say
	// they had read what they had not been shown.
	ErrStaleTerms = errors.New("wallet: those are not the terms in force")
	// ErrAlreadyJoined reports a member who is already in cashback. Their
	// acceptance stands as recorded; a second one would move the date and
	// the version, which are the whole of the FR-002 record.
	ErrAlreadyJoined = errors.New("wallet: this member is already in cashback")
	// ErrNotJoined reports a member who has never opted in. Distinct from
	// having left: one is somebody the frontend shows the opt-in to, the
	// other is somebody it shows their closed participation.
	ErrNotJoined = errors.New("wallet: this member has never opted into cashback")
	// ErrNoParticipations reports a service built with nowhere to open a
	// transaction, or nothing to read from.
	ErrNoParticipations = errors.New("wallet: participation needs a database to open a transaction on and to read from")
	// ErrNotJoinable reports a participation change the database refused
	// for a reason that is not one of the verdicts above.
	ErrNotJoinable = errors.New("wallet: the participation could not be changed")
)

// Terms is what this deployment's brand says a member is accepting: which
// brand they are joining, which revision of the terms is in force, and the
// currency their wallet is denominated in.
//
// A value rather than the brand definition itself, so this module depends
// on the three fields it uses instead of on every field a brand has. The
// composition root reads the brand file and fills this in; the zero value
// means no brand is configured, which is what ErrNoBrand reports.
type Terms struct {
	// Brand is the brand's stable identifier, written into every
	// participation as the tenant boundary ADR-0004 draws.
	Brand string
	// Version is the revision of the terms in force, and the only value an
	// acceptance may name.
	Version string
	// Currency is the brand's default currency, recorded on the
	// participation at the moment of acceptance.
	Currency money.Currency
}

// Configured reports whether a brand definition was supplied. All three
// fields or none: a brand with a terms version and no currency is a brand
// file that would not have validated, so a partial value here means a
// caller assembled one by hand.
func (t Terms) Configured() bool {
	return t.Brand != "" && t.Version != "" && t.Currency.Valid()
}

// Beginner opens the transaction a participation change and its event
// share. *pgxpool.Pool satisfies it, and so does pgx.Tx - a transaction
// begun inside a transaction is a savepoint, which is what lets a test run
// this against the real schema and roll the whole thing back.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Enrolments is the read this needs outside a transaction: one member's
// own participation. Named here per the boundary rules; *store.Queries
// satisfies it over a pool.
type Enrolments interface {
	ParticipationFor(ctx context.Context, accountID pgtype.UUID) (store.CashbackParticipation, error)
}

// Participations is the member's opt-in, as a service.
type Participations struct {
	db         Beginner
	enrolments Enrolments
	announcer  *Announcer
	terms      Terms
}

// NewParticipations builds the service. The terms may be the zero Terms -
// see [ErrNoBrand] for why that is discovered on the join rather than here.
//
// The announcer is built here rather than injected, because there is no
// choice to make: a service that recorded acceptances without publishing
// them would be the code path contracts/events.md says may not exist, and
// leaving it to a caller to pass one is leaving it to a caller to forget.
func NewParticipations(db Beginner, enrolments Enrolments, terms Terms) (*Participations, error) {
	if db == nil || enrolments == nil {
		return nil, ErrNoParticipations
	}
	announcer, err := NewAnnouncer()
	if err != nil {
		return nil, err
	}
	return &Participations{db: db, enrolments: enrolments, announcer: announcer, terms: terms}, nil
}

// Terms answers the terms this deployment is offering, so a caller can
// tell a member what they would be accepting without asking them to guess.
func (p *Participations) Terms() Terms { return p.terms }

// Of reads one member's own participation, reporting ErrNotJoined when
// there is none.
//
// No brand is consulted. What the row says is what the member accepted,
// which may be an earlier version of the terms or an earlier brand
// entirely, and answering with today's configuration instead would be
// showing them a consent they never gave.
func (p *Participations) Of(ctx context.Context, member uuid.UUID) (Participation, error) {
	row, err := p.enrolments.ParticipationFor(ctx, pgtype.UUID{Bytes: member, Valid: true})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Participation{}, ErrNotJoined
	case err != nil:
		return Participation{}, fmt.Errorf("%w: reading %s's participation: %w", ErrNotJoinable, member, err)
	}
	return participationFrom(row)
}

// Join records one member's acceptance of the terms in force, or their
// re-join after having left.
//
// The version the member offers is checked against the deployment's before
// any database work: a stale consent is never recorded, and refusing it
// here means the refusal costs no transaction.
func (p *Participations) Join(ctx context.Context, member uuid.UUID, version string) (Participation, error) {
	if !p.terms.Configured() {
		return Participation{}, ErrNoBrand
	}
	if strings.TrimSpace(version) != p.terms.Version {
		return Participation{}, ErrStaleTerms
	}

	tx, err := p.db.Begin(ctx)
	if err != nil {
		return Participation{}, fmt.Errorf("%w: %w", ErrNotJoinable, err)
	}
	// Rolls back everything that did not commit, and is a no-op after a
	// commit. Deferred rather than written on each path, because the paths
	// that return early are exactly the ones that would forget.
	defer func() { _ = tx.Rollback(ctx) }()

	row, err := store.New(tx).OptIntoParticipation(ctx, store.OptIntoParticipationParams{
		AccountID:       pgtype.UUID{Bytes: member, Valid: true},
		BrandID:         p.terms.Brand,
		TermsVersion:    p.terms.Version,
		DefaultCurrency: string(p.terms.Currency),
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The upsert's predicate found an active member and updated
		// nothing. Nothing was written, so there is nothing to announce and
		// nothing to commit.
		return Participation{}, ErrAlreadyJoined
	case err != nil:
		return Participation{}, fmt.Errorf("%w: recording %s's acceptance: %w", ErrNotJoinable, member, err)
	}

	joined, err := participationFrom(row)
	if err != nil {
		return Participation{}, err
	}
	// In the transaction that wrote the row, and fatal when it fails: the
	// append reports a key collision as a unique violation, which has
	// already aborted this transaction, so anything short of returning
	// would commit nothing while telling the member they had joined.
	if err := p.announcer.Started(ctx, tx, joined); err != nil {
		return Participation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Participation{}, fmt.Errorf("%w: recording %s's acceptance: %w", ErrNotJoinable, member, err)
	}
	return joined, nil
}

// Leave closes one member's participation (FR-003).
//
// Idempotent, and deliberately so: DELETE is the one method a client
// retries without thinking, and a member who is already gone is in the
// state they asked for. What a repeat must NOT do is announce a second
// departure, which is why the statement narrows on status = 'active' and
// this reads the row back only when it closed nothing.
//
// Nothing financial moves. Entries continue to resolve, payouts already
// made stand, and the row is closed rather than deleted - 0017's guard
// refuses the delete outright, so this is the only shape leaving can take.
func (p *Participations) Leave(ctx context.Context, member uuid.UUID) (Participation, error) {
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return Participation{}, fmt.Errorf("%w: %w", ErrNotJoinable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	row, err := store.New(tx).LeaveParticipation(ctx, pgtype.UUID{Bytes: member, Valid: true})
	if errors.Is(err, pgx.ErrNoRows) {
		return p.alreadyGone(ctx, tx, member)
	}
	if err != nil {
		return Participation{}, fmt.Errorf("%w: closing %s's participation: %w", ErrNotJoinable, member, err)
	}

	left, err := participationFrom(row)
	if err != nil {
		return Participation{}, err
	}
	if err := p.announcer.Ended(ctx, tx, left); err != nil {
		return Participation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Participation{}, fmt.Errorf("%w: closing %s's participation: %w", ErrNotJoinable, member, err)
	}
	return left, nil
}

// alreadyGone answers a leave that closed nothing: either the member left
// earlier, which is the same answer as this request asking for, or they
// never joined at all.
//
// Read inside the same transaction as the update that found nothing, so
// the two cannot disagree about a member who left between them.
func (p *Participations) alreadyGone(ctx context.Context, tx pgx.Tx, member uuid.UUID) (Participation, error) {
	row, err := store.New(tx).ParticipationFor(ctx, pgtype.UUID{Bytes: member, Valid: true})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Participation{}, ErrNotJoined
	case err != nil:
		return Participation{}, fmt.Errorf("%w: reading %s's participation: %w", ErrNotJoinable, member, err)
	}
	// No commit and no event. Nothing was written, and the departure this
	// row records was announced by whichever request actually closed it.
	return participationFrom(row)
}
