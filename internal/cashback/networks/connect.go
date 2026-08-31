// Connecting a publisher account (T145): the supported way to write the two
// rows a deployment cannot ingest without. One file, because the pair is one
// operation - a network with no account polls nothing, and an account with no
// network cannot exist.

package networks

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

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/store"
)

// ErrCannotConnect reports a connection request that does not describe an
// account, refused before anything is written.
var ErrCannotConnect = errors.New("networks: the request does not describe a publisher account to connect")

// ConnectRequest is everything needed to connect one publisher account: what
// the network publishes about itself, and the three facts that are this
// deployment's rather than the network's.
type ConnectRequest struct {
	// Network is what the adapter documents, which seeds the network row
	// when there is not one already.
	Network Documented
	// ExternalPublisherID is the publisher's own identifier at the network -
	// NETWORK_ACCOUNT_ID, the value that appears in deeplinks. Not a secret.
	ExternalPublisherID string
	// CredentialRef is a KEY INTO CONFIGURATION naming where this account's
	// credential lives, never the credential (ADR-0003). NETWORK_API_KEY is
	// what a deployment of this binary passes.
	CredentialRef string
	// BackfillFrom is the instant this account's first poll reads from, and
	// the floor the trailing re-read walks from until its own cursor exists
	// (0023). Required, because nothing in the system can work out where
	// history starts: too recent silently skips history nobody notices is
	// missing, too old asks a network for years of it. Only the operator
	// connecting the account knows.
	//
	// It is used ONLY when the account row is created. See the note on
	// [Connection.BackfillFrom].
	BackfillFrom time.Time
	// Active is whether this network and account should be live, and is the
	// operator's intent expressed by running the command. It is written on
	// every run, so the same operation both connects and pauses.
	Active bool
}

// Validate refuses a request that could not produce a working account,
// wrapping [ErrCannotConnect].
func (r ConnectRequest) Validate() error {
	if err := r.Network.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrCannotConnect, err)
	}
	if strings.TrimSpace(r.ExternalPublisherID) == "" {
		return fmt.Errorf("%w: no publisher id at %s", ErrCannotConnect, strconv.Quote(r.Network.ID.String()))
	}
	if strings.TrimSpace(r.CredentialRef) == "" {
		// Blank is refused rather than defaulted because the column exists
		// to say WHERE a credential lives, and a row that answered "nowhere"
		// would be a connected account nothing could authenticate as.
		return fmt.Errorf("%w: account %s at %s names no configuration key its credential is read from",
			ErrCannotConnect, strconv.Quote(r.ExternalPublisherID), strconv.Quote(r.Network.ID.String()))
	}
	if r.BackfillFrom.IsZero() {
		return fmt.Errorf("%w: account %s at %s says nothing about where its history starts, and nothing else can",
			ErrCannotConnect, strconv.Quote(r.ExternalPublisherID), strconv.Quote(r.Network.ID.String()))
	}
	return nil
}

// Connection is what connecting left behind: the account as the row now
// stands, and enough about how it got there for the operator to see whether
// the command did what they meant.
type Connection struct {
	// Account is the row, built from what came back rather than from what
	// was asked for - the same rule [FindPublisherAccount] follows, because
	// an account assembled from the caller's own strings would agree with
	// the caller by tautology.
	Account PublisherAccount
	// NetworkCreated and AccountCreated say which rows this run made, so a
	// re-run reads as a re-run.
	NetworkCreated bool
	AccountCreated bool
	// BackfillFrom is what the row now carries, which on a re-run is what it
	// already carried and NOT what was asked for. 0023's note is the reason:
	// the trailing re-read walks from that instant for about a hundred days,
	// so moving it forward on a polling account would leave the span between
	// the old start and the new one never re-read, and the trailing sweep is
	// the only path from pending to confirmed. Reported so a caller can say
	// out loud that it did not move.
	BackfillFrom time.Time
	// Active is what the rows now carry. Both are set together: an active
	// account at an inactive network polls, and every offer on it is
	// unclickable, because cashback.offer's read joins on n.active.
	Active bool
}

// ConnectPublisherAccount writes the network row and the publisher account
// row, and is safe to run again.
//
// It is the supported alternative to hand-written SQL against production,
// which until now was the only way these rows existed. Both statements are
// upserts, because connecting is an operator action that has to be repeatable
// - after a typo, from a fresh container, out of a deployment's init job -
// and an operator who cannot re-run a command runs a DELETE first.
//
// Pass a transaction. The two rows are one fact and a caller that committed
// the network without the account would leave a deployment configured for a
// network it has no account at, which is the state this function exists to
// remove.
//
// What it never touches: either cursor, and backfill_from on an account that
// already has one. Those are the poller's, advanced only inside the
// transaction that persisted a window (FR-031), and a connect command that
// reset one would skip every window between.
func ConnectPublisherAccount(ctx context.Context, db store.DBTX, req ConnectRequest) (Connection, error) {
	if db == nil {
		return Connection{}, ErrNoPollerStore
	}
	if err := req.Validate(); err != nil {
		return Connection{}, err
	}

	q := store.New(db)
	networkExisted, err := networkRowExists(ctx, q, req.Network.ID)
	if err != nil {
		return Connection{}, err
	}
	accountExisted, err := accountRowExists(ctx, q, req.Network.ID, req.ExternalPublisherID)
	if err != nil {
		return Connection{}, err
	}

	if _, err := q.EnsureNetwork(ctx, store.EnsureNetworkParams{
		ID:                 req.Network.ID.String(),
		DisplayName:        req.Network.DisplayName,
		ClickRefParam:      req.Network.ClickRefParam,
		MaxQueryWindowDays: int32(req.Network.MaxQueryWindowDays), //nolint:gosec // Validate refused anything but a positive count of days.
		RateLimitPerMinute: int32(req.Network.RateLimitPerMinute), //nolint:gosec // Validate refused anything but a positive rate.
		Active:             req.Active,
	}); err != nil {
		return Connection{}, fmt.Errorf("networks: writing the network row for %s: %w",
			strconv.Quote(req.Network.ID.String()), err)
	}

	row, err := q.ConnectNetworkAccount(ctx, store.ConnectNetworkAccountParams{
		NetworkID:           req.Network.ID.String(),
		ExternalPublisherID: req.ExternalPublisherID,
		CredentialRef:       req.CredentialRef,
		BackfillFrom:        pgtype.Timestamptz{Time: req.BackfillFrom.UTC(), Valid: true},
		Active:              req.Active,
	})
	if err != nil {
		return Connection{}, fmt.Errorf("networks: connecting publisher account %s at %s: %w",
			strconv.Quote(req.ExternalPublisherID), strconv.Quote(req.Network.ID.String()), err)
	}

	account, err := NewPublisherAccount(uuid.UUID(row.ID.Bytes), NetworkID(row.NetworkID), row.ExternalPublisherID)
	if err != nil {
		return Connection{}, err
	}
	return Connection{
		Account:        account,
		NetworkCreated: !networkExisted,
		AccountCreated: !accountExisted,
		BackfillFrom:   row.BackfillFrom.Time,
		Active:         row.Active,
	}, nil
}

// networkRowExists reports whether the network row was already there, read in
// the caller's transaction so the answer is the one the upsert then acts on.
func networkRowExists(ctx context.Context, q *store.Queries, id NetworkID) (bool, error) {
	_, err := q.GetNetwork(ctx, id.String())
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("networks: reading the network row for %s: %w", strconv.Quote(id.String()), err)
	}
	return true, nil
}

// accountRowExists reports whether the publisher account row was already
// there, for the same reason and in the same transaction.
func accountRowExists(ctx context.Context, q *store.Queries, network NetworkID, externalID string) (bool, error) {
	_, err := q.GetNetworkAccountByPublisher(ctx, store.GetNetworkAccountByPublisherParams{
		NetworkID:           network.String(),
		ExternalPublisherID: externalID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("networks: reading the publisher account %s at %s: %w",
			strconv.Quote(externalID), strconv.Quote(network.String()), err)
	}
	return true, nil
}
