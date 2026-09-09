package main

// Driving the event stream's consumers (T126).
//
// The registry has been complete and tested since T018 and, until now, has
// never run: nothing in the tree called Subscribe, and nothing ticked it.
// This is where that changes, and the shape is the one the registry's own
// documentation asks for - "drive it by calling Tick on a schedule... the
// advisory lock there is what keeps two instances from delivering at once".

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/platform/events"
	"github.com/Nomos-N4s/apivo-news/internal/platform/scheduler"
)

const (
	// subscriberJobName identifies the delivery pass in the scheduler and
	// names its fleet-wide lock. Stable across releases, like every other
	// job name: two instances exclude each other only while they agree on
	// it, and two instances delivering at once is the one thing the lock
	// is here to stop.
	subscriberJobName = "domain-event-subscribers"
	// subscriberInterval is how long a fact may sit in the stream before a
	// consumer sees it. Half a minute is chosen against what the only
	// subscriber does: an account deleted upstream should stop being able
	// to click through the catalogue promptly, and nothing here is so
	// urgent that a tighter loop would buy anything but polls.
	subscriberInterval = 30 * time.Second
	// subscriberTimeout bounds one pass. A pass is a bounded batch per
	// subscriber, so one still going after two minutes is not delivering,
	// it is wedged - and a wedged pass must free its lock for the next
	// tick rather than stopping delivery fleet-wide.
	subscriberTimeout = 2 * time.Minute
)

// registerSubscribers builds the process's event registry, subscribes every
// consumer to it, puts one delivery pass on the scheduler, and answers how
// many jobs that added for the capacity check.
//
// Gated with the rest of cashback, because cashback's account closures are
// the only subscriber there is. That is a fact about today rather than a
// design: the registry is a platform service, and the first subscriber
// outside cashback moves this call out of that block and up beside the
// pool. Until then, ticking a registry with nothing in it in a deployment
// that has cashback switched off would be a job holding a lock to do
// nothing.
//
// A nil consumer is not an error, for the reason a nil settlement sweep is
// not: it means the authenticated surface was not built, or cashback is
// off, and then there is nothing subscribed to deliver to.
func registerSubscribers(ctx context.Context, log *slog.Logger, jobs *scheduler.Scheduler, db *pgxpool.Pool, closures *wallet.AccountClosures, counted func(context.Context, events.DeadLetter)) (int, error) {
	if closures == nil {
		return 0, nil
	}
	registry := events.NewRegistry(db, events.RegistryConfig{
		// A parked delivery is money or consent nobody acted on, and the
		// dead-letter table is durable but silent. This is the line that
		// says it happened at the moment it happens, with what an operator
		// needs to find the row - and, since #618, the number an alert can
		// watch without anybody reading the line.
		//
		// The log carries the identifiers and the measurement carries the
		// labels, which is the division that keeps both useful: an event id
		// is what an operator needs to find one row and the last thing a
		// metric should be grouped by.
		OnDeadLetter: func(parked events.DeadLetter) {
			log.ErrorContext(ctx, "an event delivery is parked and its lane is blocked until an operator requeues it",
				"subscriber", parked.Subscriber, "event", parked.Event.EventID,
				"type", parked.Event.Type, "attempts", parked.Attempts, "last_error", parked.LastError)
			if counted != nil {
				counted(ctx, parked)
			}
		},
	})
	if err := closures.Subscribe(registry); err != nil {
		return 0, err
	}
	if err := jobs.Register(scheduler.Job{
		Name:     subscriberJobName,
		Interval: subscriberInterval,
		Timeout:  subscriberTimeout,
		Run:      registry.Tick,
	}); err != nil {
		return 0, err
	}
	log.InfoContext(ctx, "event subscribers registered",
		"job", subscriberJobName, "interval", subscriberInterval,
		"subscriber", wallet.AccountClosuresSubscriber, "type", wallet.TypeAccountDeleted)
	return 1, nil
}
