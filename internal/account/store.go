package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
