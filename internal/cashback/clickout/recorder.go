// Writing a click and announcing it as one commit (T076).
//
// [Clicks] does the insert and knows nothing about the outbox, which is
// right: it is the module's evidence writer and the outbox is a table it
// does not own. What is missing from it is the transaction, and a
// transaction is not something a type built once over a pool can have - so
// it is opened here, per click, around both writes.
//
// This is the only place in the module that opens one. [ClickOuts.Issue]
// deliberately builds the redirect BEFORE recording, so an adapter call
// never happens inside a transaction; keeping the transaction down here is
// what preserves that.

package clickout

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout/store"
)

// ErrNoTransactions reports a recorder built with nowhere to open a
// transaction. Refused at construction, because one that discovered it later
// would discover it with a member waiting on a redirect.
var ErrNoTransactions = errors.New("clickout: recording a click needs somewhere to open a transaction")

// Beginner opens the transaction a click and its event share.
//
// *pgxpool.Pool satisfies it, and so does pgx.Tx - a transaction begun
// inside a transaction is a savepoint, which is what lets a test run this
// against the real schema and roll the whole thing back.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// AnnouncedClicks records a click and announces it, in one transaction.
type AnnouncedClicks struct {
	db        Beginner
	announcer *Announcer
}

// NewAnnouncedClicks builds the recorder, refusing one with nowhere to open
// a transaction.
//
// The announcer is built here rather than injected, because there is no
// choice to make: a recorder that wrote clicks without publishing them would
// be the code path contracts/events.md says may not exist, and leaving it to
// a caller to pass one is leaving it to a caller to forget.
func NewAnnouncedClicks(db Beginner) (*AnnouncedClicks, error) {
	if db == nil {
		return nil, ErrNoTransactions
	}
	announcer, err := NewAnnouncer()
	if err != nil {
		return nil, err
	}
	return &AnnouncedClicks{db: db, announcer: announcer}, nil
}

// Record writes the click and appends its event, and answers what the
// database recorded.
//
// The order inside is insert then announce, and it has to be: the event
// carries the row's own id and the row's own instant, and neither exists
// until the insert has returned. What makes that safe is the transaction -
// a refused append rolls the click back with it, so the failure a member
// sees is a redirect that was not issued rather than a click that will never
// be announced.
func (a *AnnouncedClicks) Record(ctx context.Context, click NewClick) (Click, error) {
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return Click{}, fmt.Errorf("%w: %w", ErrNotRecorded, err)
	}
	// Rolls back everything that did not commit, and is a no-op after a
	// commit. Deferred rather than written on each path, because the paths
	// that return early are exactly the ones that would forget.
	defer func() { _ = tx.Rollback(ctx) }()

	clicks, err := NewClicks(store.New(tx))
	if err != nil {
		return Click{}, err
	}
	recorded, err := clicks.Record(ctx, click)
	if err != nil {
		return Click{}, err
	}
	if err := a.announcer.Created(ctx, tx, recorded); err != nil {
		return Click{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Click{}, fmt.Errorf("%w: %s: %w", ErrNotRecorded, click.Ref, err)
	}
	return recorded, nil
}
