package main

// The event registry's first wiring (T126), proved where the mistake would
// actually be made.
//
// Both halves of a subscription are already covered elsewhere: the registry
// delivers and parks correctly in its own package's tests, and the handler
// closes participations and flags withdrawals in the wallet's. What neither
// can prove is that this process ever calls one from the other - a
// subscriber built and never registered is the exact failure mode
// registerSettlement's comment describes, and it fails silently, because
// every service it depends on still works.

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	walletstore "github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/scheduler"
)

// aConsumer builds the account-closure consumer over the pool, the way the
// composition root does.
func aConsumer(t *testing.T, pool *pgxpool.Pool) *wallet.AccountClosures {
	t.Helper()
	participations, err := wallet.NewParticipations(pool, walletstore.New(pool), wallet.Terms{})
	if err != nil {
		t.Fatalf("NewParticipations(): %v", err)
	}
	closures, err := wallet.NewAccountClosures(discardLogger(), pool, participations, walletstore.New(pool))
	if err != nil {
		t.Fatalf("NewAccountClosures(): %v", err)
	}
	return closures
}

// TestTheSubscriberDeliveryPassIsRegisteredAndRunnable walks the wiring to
// the one thing that cannot be checked by reading it: that the registered
// job runs against the real schema. A tick reads the stream and saves a
// checkpoint, so a subscriber whose durable state had no table to live in
// would fail here rather than at the first deletion in production.
func TestTheSubscriberDeliveryPassIsRegisteredAndRunnable(t *testing.T) {
	t.Parallel()
	ctx, pool := opsWiringPool(t)

	jobs := scheduler.New(discardLogger(), lockerFor(t, pool), scheduler.Config{})
	registered, err := registerSubscribers(ctx, discardLogger(), jobs, pool, aConsumer(t, pool), nil)
	if err != nil {
		t.Fatalf("registerSubscribers(): %v", err)
	}
	if registered != 1 {
		t.Fatalf("registerSubscribers() added %d job(s), want 1 - the capacity check is sized from this number", registered)
	}

	ran, err := jobs.RunOnce(ctx, subscriberJobName)
	if err != nil {
		t.Fatalf("RunOnce(%s): %v", subscriberJobName, err)
	}
	if !ran {
		t.Errorf("the delivery pass did not run; every event in the stream would wait forever")
	}
}

// TestTheDeliveryPassIsRegisteredUnderOneName. The job name is the
// fleet-wide lock, so two registrations under it would be two instances
// believing they excluded each other while both delivered.
func TestTheDeliveryPassIsRegisteredUnderOneName(t *testing.T) {
	t.Parallel()
	ctx, pool := opsWiringPool(t)

	jobs := scheduler.New(discardLogger(), lockerFor(t, pool), scheduler.Config{})
	if _, err := registerSubscribers(ctx, discardLogger(), jobs, pool, aConsumer(t, pool), nil); err != nil {
		t.Fatalf("the first registration: %v", err)
	}
	if _, err := registerSubscribers(ctx, discardLogger(), jobs, pool, aConsumer(t, pool), nil); err == nil {
		t.Error("a second delivery pass registered under the same name, so two would share one lock")
	}
}

// TestNoConsumerRegistersNoJob. A deployment with cashback off has nothing
// subscribed, and a job holding a fleet-wide lock to deliver to nobody is a
// connection out of the pool for no work.
func TestNoConsumerRegistersNoJob(t *testing.T) {
	t.Parallel()
	ctx, pool := opsWiringPool(t)

	jobs := scheduler.New(discardLogger(), lockerFor(t, pool), scheduler.Config{})
	registered, err := registerSubscribers(ctx, discardLogger(), jobs, pool, nil, nil)
	if err != nil {
		t.Fatalf("registerSubscribers(nil): %v", err)
	}
	if registered != 0 {
		t.Errorf("registerSubscribers(nil) added %d job(s), want none", registered)
	}
	if _, err := jobs.RunOnce(ctx, subscriberJobName); err == nil {
		t.Error("the delivery pass is registered although nothing subscribed")
	}
}

// TestTheConsumerSubscribesToTheTypeIdentityPublishes pins the one string
// that couples two products. cashback subscribes by type, identity appends
// by type, and neither compiler sees the other: a typo here is a
// subscription that never fires and a deletion nobody acts on, with nothing
// failing anywhere.
func TestTheConsumerSubscribesToTheTypeIdentityPublishes(t *testing.T) {
	t.Parallel()

	// contracts/events.md, verbatim.
	if wallet.TypeAccountDeleted != "identity.account.deleted" {
		t.Errorf("the consumer subscribes to %q, want the contract's identity.account.deleted", wallet.TypeAccountDeleted)
	}
}

// lockerFor builds the advisory locker the scheduler takes, on a pool the
// test already owns.
func lockerFor(t *testing.T, pool *pgxpool.Pool) scheduler.Locker {
	t.Helper()
	return scheduler.NewAdvisoryLocker(pool, scheduler.LockerConfig{})
}
