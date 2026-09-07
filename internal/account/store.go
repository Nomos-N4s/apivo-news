package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is the database seam, satisfied by the platform pool. Named here
// rather than imported so this module depends on a shape it uses, not on
// pgxpool.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// PGStore is the database-backed TourStore, reading and writing
// account.tour_progress (migration 0009).
type PGStore struct {
	db Querier
}

// NewPGStore builds the store the composition root wires.
func NewPGStore(db Querier) PGStore { return PGStore{db: db} }

// Tours returns the account's progress document.
func (s PGStore) Tours(ctx context.Context, accountID uuid.UUID) (map[string]string, error) {
	var raw []byte
	err := s.db.QueryRow(ctx,
		`select tour_progress from account where id = $1`,
		accountID.String()).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNoAccount, accountID)
	}
	if err != nil {
		return nil, fmt.Errorf("account: reading tour progress for %s: %w", accountID, err)
	}
	tours := map[string]string{}
	// The column is constrained to an object, but every value inside it was
	// written by a client. A document that will not decode into
	// map[string]string is one somebody put a nested value into; reporting
	// it beats handing the caller half a document.
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &tours); err != nil {
			return nil, fmt.Errorf("account: tour progress for %s is not a flat object: %w", accountID, err)
		}
	}
	return tours, nil
}

// SetTour records one cursor, refusing to add a NEW tour once the account
// holds MaxToursPerAccount of them. Updating one already present is always
// allowed — the cap bounds how many keys exist, not how often they move.
//
// One statement rather than read-modify-write: two editors on two devices
// finishing the same tour at once would otherwise race, and the loser's
// write would silently vanish. The CASE decides the cap inside the same
// snapshot that applies the update.
func (s PGStore) SetTour(ctx context.Context, accountID uuid.UUID, tourID, cursor string) (bool, error) {
	var stored bool
	err := s.db.QueryRow(ctx,
		`update account
		    set tour_progress = case
		        when jsonb_exists(tour_progress, $2)
		          or (select count(*) from jsonb_object_keys(tour_progress)) < $4
		        then jsonb_set(tour_progress, array[$2], to_jsonb($3::text), true)
		        else tour_progress
		    end
		  where id = $1
		  returning tour_progress ->> $2 is not distinct from $3`,
		accountID.String(), tourID, cursor, MaxToursPerAccount).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("%w: %s", ErrNoAccount, accountID)
	}
	if err != nil {
		return false, fmt.Errorf("account: recording tour progress for %s: %w", accountID, err)
	}
	return stored, nil
}

// Register creates the caller's row as a reader, or hands back the row
// already there.
//
// One INSERT with ON CONFLICT (id) DO NOTHING, so two sign-ins racing on
// the same first visit cannot both create and neither can fail: the loser
// reads the winner's row. The conflict clause names the id and not the
// email on purpose — the id is the identity, the email is a fact about it —
// so an email already held by a DIFFERENT id is a unique violation, which
// is reported as ErrEmailTaken rather than absorbed. The role is written
// here as a literal and never taken from the caller: a registration
// produces a reader, and every other role is somebody else's decision.
func (s PGStore) Register(ctx context.Context, id uuid.UUID, email string) (Profile, bool, error) {
	email = strings.TrimSpace(email)
	var p Profile
	err := s.db.QueryRow(ctx,
		`insert into account (id, email, display_name, role)
		 values ($1, $2, $3, 'reader')
		 on conflict (id) do nothing
		 returning id, email, display_name, role`,
		id.String(), email, displayNameFor(email)).Scan(&p.ID, &p.Email, &p.DisplayName, &p.Role)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		existing, err := s.Profile(ctx, id)
		return existing, false, err
	case isUniqueViolation(err):
		return Profile{}, false, fmt.Errorf("%w: %s", ErrEmailTaken, email)
	case err != nil:
		return Profile{}, false, fmt.Errorf("account: registering %s: %w", id, err)
	}
	return p, true, nil
}

// Profile reads the caller's own row.
func (s PGStore) Profile(ctx context.Context, id uuid.UUID) (Profile, error) {
	var p Profile
	err := s.db.QueryRow(ctx,
		`select id, email, display_name, role from account where id = $1`,
		id.String()).Scan(&p.ID, &p.Email, &p.DisplayName, &p.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return Profile{}, fmt.Errorf("%w: %s", ErrNoAccount, id)
	}
	if err != nil {
		return Profile{}, fmt.Errorf("account: reading account %s: %w", id, err)
	}
	return p, nil
}

// displayNameFor is the name a registration starts with: the local part of
// the email, because the column may not be blank and nothing else about
// the person is known yet. Not the whole address — a display name is
// printed wherever a name is, and an email in a name slot is an address
// leaked everywhere a name appears.
func displayNameFor(email string) string {
	if local, _, ok := strings.Cut(email, "@"); ok && strings.TrimSpace(local) != "" {
		return strings.TrimSpace(local)
	}
	return email
}

// isUniqueViolation reports SQLSTATE 23505, the one error Register expects
// from the schema rather than from a fault.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
