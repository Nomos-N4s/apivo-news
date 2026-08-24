package scheduler

// The Postgres implementation of Locker: session-scoped advisory locks, one
// per job name.
//
// Session-scoped and not transaction-scoped because a job is not a
// transaction - a poller makes network calls and writes several transactions
// inside one run, and pg_advisory_xact_lock would be gone at the first commit.
// The cost of that choice is that the lock outlives every statement and is
// released only by an explicit unlock or by the backend dying, so the lock is
// taken on a connection acquired for exactly one run and released with it,
// never on whichever pooled connection the job's own queries happen to get.

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// lockNamespace is the first key of every scheduler lock. Advisory locks in
// the two-key space are disjoint from those in the one-key space, and the
// namespace separates scheduler jobs from any other two-key user, so a job
// name can never collide with a lock taken elsewhere in the application - the
// ingestion poll loop's "apivo.poll", for instance.
const lockNamespace = "apivo.scheduler"

// AdvisoryLocker takes Postgres session-scoped advisory locks, one per job
// name, so that two application instances never run the same job at once.
//
// Job names are mapped onto the integer key space Postgres locks on with
// hashtext(), so they are hashed rather than stored and only ever need to stay
// equal to themselves. Two names that hashed alike would exclude each other -
// they would take turns rather than run together - which degrades throughput
// and never correctness.
type AdvisoryLocker struct {
	pool *pgxpool.Pool
}

var (
	_ Locker = (*AdvisoryLocker)(nil)
	_ Lock   = (*advisoryLock)(nil)
)

// NewAdvisoryLocker builds a locker over the process's connection pool.
//
// Each held lock keeps one connection out of the pool for the length of the
// run it guards, so the pool must have room for the concurrent jobs plus the
// work those jobs do.
func NewAdvisoryLocker(pool *pgxpool.Pool) *AdvisoryLocker {
	return &AdvisoryLocker{pool: pool}
}

// TryLock takes the advisory lock for name without waiting, on a connection
// acquired for the caller to hold until it releases the lock. It reports false
// and no error when another session holds the lock, which is the ordinary
// outcome on every instance but one.
func (l *AdvisoryLocker) TryLock(ctx context.Context, name string) (Lock, bool, error) {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("scheduler: job %q: acquire lock connection: %w", name, err)
	}

	var held bool
	err = conn.QueryRow(ctx,
		`select pg_try_advisory_lock(hashtext($1::text), hashtext($2::text))`,
		lockNamespace, name).Scan(&held)
	if err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("scheduler: job %q: take advisory lock: %w", name, err)
	}
	if !held {
		conn.Release()
		return nil, false, nil
	}
	return &advisoryLock{conn: conn, name: name}, true, nil
}

// advisoryLock is one held session-scoped lock and the connection its session
// belongs to.
type advisoryLock struct {
	conn *pgxpool.Conn
	name string

	// once makes Release idempotent: the connection underneath must be
	// handed back to the pool exactly once, whatever the caller does.
	once sync.Once
	err  error
}

// Release unlocks the advisory lock and returns its connection to the pool.
// It is idempotent - later calls repeat the first call's result - so a caller
// may release defensively as well as on its normal path.
//
// A lock that cannot be unlocked cleanly takes its connection with it: the
// connection is closed rather than pooled, because Postgres frees a session's
// advisory locks when its backend dies, and a still-locked session handed back
// to the pool would block this job on every instance until the process ended.
func (l *advisoryLock) Release(ctx context.Context) error {
	l.once.Do(func() {
		defer l.conn.Release()

		var released bool
		err := l.conn.QueryRow(ctx,
			`select pg_advisory_unlock(hashtext($1::text), hashtext($2::text))`,
			lockNamespace, l.name).Scan(&released)
		switch {
		case err != nil:
			l.err = fmt.Errorf("scheduler: job %q: release advisory lock: %w", l.name, err)
		case !released:
			// Postgres reports false when this session did not hold the
			// lock, which means the session was replaced underneath us.
			l.err = fmt.Errorf("scheduler: job %q: advisory lock was not held at release", l.name)
		default:
			return
		}
		// Closed under the caller's context, so a release that is already
		// out of time abandons the socket instead of waiting for a clean
		// goodbye. Release, deferred above, destroys a closed connection
		// rather than pooling it.
		_ = l.conn.Conn().Close(ctx)
	})
	return l.err
}
