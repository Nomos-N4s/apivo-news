// The publisher account an adapter is configured to poll, as the database
// holds it (T057).

package networks

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/store"
)

// ErrAccountNotConnected reports a configuration naming a publisher account
// that no row describes.
//
// It is not the same failure as [ErrInvalidPublisherAccount], and the
// difference decides who fixes it. An invalid account is a value nobody could
// have meant; an unconnected one is a well-formed name for an account
// somebody has not created yet, which is the ordinary state of a deployment
// configured ahead of the operator action that connects it.
var ErrAccountNotConnected = errors.New("networks: no publisher account is connected under that name")

// ConnectedAccount is a publisher account as the database holds it: the
// account an adapter polls, and the two facts about it a caller cannot see
// from the account alone.
//
// Both are carried rather than obeyed. An account is born inactive (0011)
// and starts with no backfill start (0023), and the poller refuses either on
// every poll - so a caller that declined to build the sweeps for one would
// need a restart to pick up the operator's fix, which is a worse trade than
// a log line that names what is missing.
type ConnectedAccount struct {
	// Account is what the poller polls and what every evidence row carries.
	Account PublisherAccount
	// Active is the row's own switch.
	Active bool
	// BackfillFrom is where this account's history starts, zero when nobody
	// has said. The poller refuses a zero one by name.
	BackfillFrom time.Time
}

// Pollable reports whether the account is in a state the poller will accept:
// switched on, and with a start somebody chose. It is what a composition
// root asks before it says, in one line, whether ingestion will actually
// happen.
func (c ConnectedAccount) Pollable() bool { return c.Active && !c.BackfillFrom.IsZero() }

// FindPublisherAccount resolves the account a deployment's configuration
// names - a network and the publisher's own identifier at it - into the row
// that owns the cursors.
//
// It exists so the composition root can wire a poller without touching this
// package's generated store, which is the rule every other module here
// follows: a module exposes a constructor over the pool, and cmd never
// learns a query. Both a pool and a transaction satisfy the parameter, so
// the wiring can be exercised inside a transaction that is rolled back.
func FindPublisherAccount(ctx context.Context, db store.DBTX, network NetworkID, externalID string) (ConnectedAccount, error) {
	if db == nil {
		return ConnectedAccount{}, ErrNoPollerStore
	}
	row, err := store.New(db).GetNetworkAccountByPublisher(ctx, store.GetNetworkAccountByPublisherParams{
		NetworkID:           string(network),
		ExternalPublisherID: externalID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ConnectedAccount{}, fmt.Errorf("%w: %s at %s",
			ErrAccountNotConnected, strconv.Quote(externalID), strconv.Quote(string(network)))
	case err != nil:
		return ConnectedAccount{}, fmt.Errorf("networks: reading the publisher account %s at %s: %w",
			strconv.Quote(externalID), strconv.Quote(string(network)), err)
	}

	// Built from the ROW rather than from the configuration that found it.
	// The two agree here by construction, and building from the row is what
	// keeps them agreeing: the poller checks the account it is handed
	// against the row it reads on every poll, and an account assembled from
	// the caller's own strings would pass that check by tautology.
	account, err := NewPublisherAccount(uuid.UUID(row.ID.Bytes), NetworkID(row.NetworkID), row.ExternalPublisherID)
	if err != nil {
		return ConnectedAccount{}, err
	}
	return ConnectedAccount{Account: account, Active: row.Active, BackfillFrom: row.BackfillFrom.Time}, nil
}
