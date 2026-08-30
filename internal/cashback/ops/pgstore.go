package ops

import (
	"context"
	"fmt"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	networksstore "github.com/Nomos-N4s/apivo-news/internal/cashback/networks/store"
)

// DBTX is the database access this module needs, named here per the
// boundary rules. A *pgxpool.Pool satisfies it, and so does a pgx.Tx.
type DBTX = networksstore.DBTX

// PGStore is the operator surface over a real database.
//
// It reads the unattributed queue through the networks module's own queue
// rather than through a query of its own. That is deliberate and not
// merely tidy: "still open" is three conditions about evidence - nobody
// resolved it, no later report replaced the one it names, nothing has been
// credited against it - and a second spelling of them here would be a
// second answer to the question of whether an operator is looking at money
// that is still theirs to decide.
type PGStore struct {
	db DBTX
}

// NewPGStore builds the store the composition root wires with the platform
// pool.
func NewPGStore(db DBTX) *PGStore { return &PGStore{db: db} }

// Open returns one page of open unattributed work.
func (s *PGStore) Open(ctx context.Context, after networks.After, limit int) ([]networks.OpenReport, error) {
	queue, err := networks.NewUnattributedQueue(networksstore.New(s.db))
	if err != nil {
		return nil, fmt.Errorf("ops: reading the unattributed queue: %w", err)
	}
	return queue.Open(ctx, after, limit)
}
