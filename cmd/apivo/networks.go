// Wiring the affiliate-network sweeps: the one place the configured adapter,
// the publisher account it polls and the poller that drives it are put
// together (T057).
//
// It is a file of its own rather than another block inside serve, because it
// is the only piece of the composition root that has to READ something
// before it can decide what to build - the account row - and that makes it
// the only piece worth testing directly rather than through a served process.

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
)

// pollerDB is what the wiring needs of the database: a transaction to
// persist a window in, and a handle to read the account row with. A
// *pgxpool.Pool satisfies both halves, and so does a transaction - which is
// what lets this wiring be exercised against one that is rolled back.
type pollerDB interface {
	networks.Beginner
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ingestionOff reports a deployment that could have polled a network and was
// not told enough to. It is an error rather than a (nil, nil) return because
// the distinction it carries is the one a caller must not lose: this is a
// deployment running with ingestion off, which is worth an ERROR line, and
// not a deployment that is fine.
//
// It is deliberately not fatal. Cashback's own stance is that the product
// starts fully configured or not at all, and that stance is about the money
// path: a half-configured LEDGER_DRIVER moves money to the wrong place. What
// is missing here is not a key of that kind - it is an operator action
// against the database, connecting a publisher account, which a deployment
// is routinely configured ahead of. Refusing to start would take the public
// news site down until somebody ran an INSERT.
type ingestionOff struct{ reason string }

func (e *ingestionOff) Error() string { return "network ingestion is off: " + e.reason }

// connectNetwork resolves the one network this deployment integrates: the
// adapter NETWORK_DRIVER names, and the publisher account row it acts for.
//
// It is a step of its own because two things need it and neither should look
// it up separately. The poller acts for the account; the click-out endpoint
// builds its redirects with the same adapter (FR-021), and a deployment
// where those two disagreed about which network it is connected to would
// issue clicks one code path could not reconcile.
//
// It returns an error wrapping [ingestionOff] when the deployment has not
// been told enough, and an ordinary error when it has been told something
// wrong - an adapter this binary does not have, an account row that cannot
// be read. The first is a state to report and carry on from; the second is a
// deployment that would never work and should not start.
func connectNetwork(ctx context.Context, cfg config.NetworkConfig, db pollerDB) (networks.Network, networks.ConnectedAccount, error) {
	switch {
	case cfg.Driver == "":
		return nil, networks.ConnectedAccount{}, &ingestionOff{reason: "NETWORK_DRIVER names no adapter"}
	case cfg.AccountID == "":
		// Required here even for the fixture adapter, which needs no
		// credentials: the cursors live on a network_account row, and this
		// is the only value that says which one. An adapter that needs no
		// credential still polls on behalf of somebody.
		return nil, networks.ConnectedAccount{}, &ingestionOff{reason: "NETWORK_ACCOUNT_ID names no publisher account at " + strconv.Quote(cfg.Driver)}
	}

	connected, err := networks.FindPublisherAccount(ctx, db, networks.NetworkID(cfg.Driver), cfg.AccountID)
	switch {
	case errors.Is(err, networks.ErrAccountNotConnected):
		// Not a typo the binary can correct: connecting a publisher account
		// is an operator action against the database, and a deployment
		// configured ahead of it is an ordinary order of events rather than
		// a broken build.
		return nil, networks.ConnectedAccount{}, &ingestionOff{reason: fmt.Sprintf("no publisher account %s is connected at %s",
			strconv.Quote(cfg.AccountID), strconv.Quote(cfg.Driver))}
	case err != nil:
		return nil, networks.ConnectedAccount{}, err
	}

	adapter, err := networkAdapter(cfg.Driver, connected.Account)
	if err != nil {
		return nil, networks.ConnectedAccount{}, err
	}
	return adapter, connected, nil
}

// newNetworkSweeps builds the scheduled polls for the connected publisher
// account.
//
// The adapter and the account row come from [connectNetwork], so what is
// left here is only the poller and the two jobs over it.
func newNetworkSweeps(ctx context.Context, log *slog.Logger, adapter networks.Network, connected networks.ConnectedAccount, db pollerDB) (*networks.Sweeps, error) {
	poller, err := networks.NewPoller(db)
	if err != nil {
		return nil, err
	}
	sweeps, err := networks.NewSweeps(log, poller, adapter)
	if err != nil {
		return nil, err
	}

	// Registered either way, and the row's own gaps reported rather than
	// obeyed. Both are fixed with one UPDATE and no restart, which is what
	// the poller refusing them per tick buys - but an operator reading the
	// startup line should not have to work out from an error stream why
	// their sweeps are failing.
	if !connected.Active {
		log.ErrorContext(ctx, "the publisher account is registered for polling but is NOT ACTIVE, so every sweep will refuse until somebody switches it on",
			"account", connected.Account.String())
	}
	if connected.BackfillFrom.IsZero() {
		log.ErrorContext(ctx, "the publisher account does not say how far back to read (cashback.network_account.backfill_from is null), so every sweep will refuse until an operator sets it",
			"account", connected.Account.String())
	}
	return sweeps, nil
}

// networkAdapter builds the adapter NETWORK_DRIVER names.
//
// An unknown name is fatal, unlike everything above it. The operator has
// said which network this deployment integrates; a binary that does not have
// that network cannot poll it, cannot be made to by configuration, and
// starting anyway would leave a deployment that believes it is connected.
//
// The set of drivers is [shippedNetworks] rather than a switch here, so that
// this answer and connect-network's cannot disagree.
func networkAdapter(driver string, account networks.PublisherAccount) (networks.Network, error) {
	entry, err := lookupNetwork(driver)
	if err != nil {
		return nil, err
	}
	return entry.construct(account)
}
