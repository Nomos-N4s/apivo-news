package scheduler_test

// The half of the scheduler that only a real Postgres can prove: that the
// advisory lock actually excludes a second instance, and that it is given up
// again however a run ends.
//
// Everything about the schedule itself - repetition, jitter, cancellation,
// panic isolation, the skip-never-queue rule - is asserted without a database
// in scheduler_test.go. What is left here is the claim the lock exists for,
// and it cannot be faked: two instances, two connection pools, one job name.
//
// These tests are keyed on DATABASE_URL, with one exception noted where it
// stands. In CI the database is a service container and they always run;
// locally they skip unless one is configured.
//
// No migration is applied. Advisory locks live outside the schema entirely -
// they are keys in a lock table, not rows - which is what lets a job take one
// before anything of its own exists.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/platform/scheduler"
)

// lockPool opens a connection pool of its own, skipping the test when no
// database is configured. Each call is one application instance: the advisory
// lock is session-scoped, so only separate pools can contend for it.
func lockPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the advisory lock")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("pinging: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// exhaustiblePool opens a pool of exactly one connection, so a test can hold
// that connection and leave the locker with nothing to acquire - the
// starvation this scheduler risks by holding a connection for the length of
// every run.
func exhaustiblePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	raw := os.Getenv("DATABASE_URL")
	if raw == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the advisory lock")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parsing DATABASE_URL: %v", err)
	}
	query := parsed.Query()
	query.Set("pool_max_conns", "1")
	parsed.RawQuery = query.Encode()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("pinging: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// jobName returns a name no other test will use, so the suite can run in
// parallel against a shared database.
func jobName(prefix string) string {
	return prefix + "-" + uuid.NewString()
}

// newInstance builds one application instance: a scheduler over its own pool,
// with a single job registered under name. The interval is an hour because
// every test here drives RunOnce, except the one that deliberately does not.
func newInstance(t *testing.T, pool *pgxpool.Pool, name string, run func(context.Context) error) *scheduler.Scheduler {
	t.Helper()
	s := scheduler.New(slog.New(slog.DiscardHandler), scheduler.NewAdvisoryLocker(pool, scheduler.LockerConfig{}), scheduler.Config{})
	if err := s.Register(scheduler.Job{Name: name, Interval: time.Hour, Run: run}); err != nil {
		t.Fatalf("Register() error: %v", err)
	}
	return s
}

// TestTryLockReportsAPoolItCannotDrawFrom needs no database: a closed pool
// refuses to hand out a connection without going near one. It is the shutdown
// case - the pool closing under a loop that is still ticking - and the
// scheduler must be told, so that it skips the run and logs, rather than
// running the job unguarded.
func TestTryLockReportsAPoolItCannotDrawFrom(t *testing.T) {
	t.Parallel()

	pool, err := pgxpool.New(context.Background(), "postgres://u:p@192.0.2.1:5432/x?connect_timeout=1")
	if err != nil {
		t.Fatalf("building a pool: %v", err)
	}
	pool.Close()

	lock, held, err := scheduler.NewAdvisoryLocker(pool, scheduler.LockerConfig{}).TryLock(context.Background(), "unreachable")
	if err == nil {
		t.Fatal("TryLock() on a closed pool: want an error, got nil")
	}
	if held || lock != nil {
		t.Errorf("TryLock() on a closed pool = lock %v, held %v; want no lock at all", lock, held)
	}
}

// unconnectedPool builds a pool with a given MaxConns and never connects it:
// pgxpool parses and configures eagerly but dials lazily, and CheckCapacity
// only reads the configuration. So this needs no database either.
func unconnectedPool(t *testing.T, maxConns int) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(),
		fmt.Sprintf("postgres://u:p@192.0.2.1:5432/x?connect_timeout=1&pool_max_conns=%d", maxConns))
	if err != nil {
		t.Fatalf("building a pool with MaxConns=%d: %v", maxConns, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestCheckCapacity needs no database: it is arithmetic over the pool's
// configuration, and it is what Scheduler.Run asks before it starts anything.
// The case that matters is the second one - the pgx default of four
// connections cannot seat two jobs - because that is the deployment this
// scheduler would otherwise deadlock.
func TestCheckCapacity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		maxConns int
		jobs     int
		cfg      scheduler.LockerConfig
		wantErr  bool
	}{
		{name: "no jobs need no connections", maxConns: 1, jobs: 0},
		{name: "the default pool seats one job exactly", maxConns: 4, jobs: 1},
		{name: "the default pool cannot seat two jobs", maxConns: 4, jobs: 2, wantErr: true},
		{name: "a pool sized for three jobs seats them", maxConns: 8, jobs: 3},
		{name: "one connection short is still short", maxConns: 7, jobs: 3, wantErr: true},
		{
			name:     "a pool the jobs do not otherwise query needs one each",
			maxConns: 5,
			jobs:     3,
			cfg:      scheduler.LockerConfig{ConnsPerJob: 1},
		},
		{
			name:     "reserving more leaves less for the jobs",
			maxConns: 8,
			jobs:     3,
			cfg:      scheduler.LockerConfig{ReservedConns: 4},
			wantErr:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			locker := scheduler.NewAdvisoryLocker(unconnectedPool(t, tt.maxConns), tt.cfg)
			err := locker.CheckCapacity(tt.jobs)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("CheckCapacity(%d) with MaxConns=%d: want an error, got nil", tt.jobs, tt.maxConns)
				}
				// The numbers are the whole value of the error: whoever
				// reads it at deploy time has to know what to set.
				for _, want := range []string{fmt.Sprintf("MaxConns=%d", tt.maxConns), fmt.Sprintf("%d jobs", tt.jobs)} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("CheckCapacity() error = %q, want it to mention %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Errorf("CheckCapacity(%d) with MaxConns=%d = %v, want nil", tt.jobs, tt.maxConns, err)
			}
		})
	}
}

func TestTryLockGivesUpWhenThePoolHasNothingFree(t *testing.T) {
	t.Parallel()

	pool := exhaustiblePool(t)
	ctx := context.Background()

	// Hold the pool's only connection, standing in for the job's own
	// queries and the request handlers competing for the same pool.
	busy, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquiring the pool's only connection: %v", err)
	}
	defer busy.Release()

	locker := scheduler.NewAdvisoryLocker(pool, scheduler.LockerConfig{AcquireTimeout: 100 * time.Millisecond})
	started := time.Now()
	lock, held, err := locker.TryLock(ctx, jobName("starved"))
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("TryLock() on an exhausted pool: want an error, got nil")
	}
	if held || lock != nil {
		t.Errorf("TryLock() on an exhausted pool = lock %v, held %v; want no lock at all", lock, held)
	}
	if !strings.Contains(err.Error(), "acquire lock connection") {
		t.Errorf("TryLock() error = %q, want it to say it could not get a connection", err)
	}
	// The assertion the bound exists for: without it pgxpool.Acquire waits
	// for a connection that is never coming, and the job's loop stops
	// ticking with nothing logged to say why.
	if elapsed > 30*time.Second {
		t.Errorf("TryLock() blocked for %v; the acquire must give up on its own", elapsed)
	}
}

func TestAdvisoryLockIsGrantedThenGivenBack(t *testing.T) {
	t.Parallel()

	pool := lockPool(t)
	locker := scheduler.NewAdvisoryLocker(pool, scheduler.LockerConfig{})
	ctx := context.Background()
	name := jobName("granted")

	lock, held, err := locker.TryLock(ctx, name)
	if err != nil {
		t.Fatalf("TryLock() error: %v", err)
	}
	if !held {
		t.Fatal("TryLock() on a free lock did not take it")
	}
	if err := lock.Release(ctx); err != nil {
		t.Fatalf("Release() error: %v", err)
	}

	// Releasing twice must be harmless: the scheduler releases on every
	// path out of a run, and a defensive second call must not double-return
	// the connection underneath.
	if err := lock.Release(ctx); err != nil {
		t.Errorf("second Release() error = %v, want nil: Release is idempotent", err)
	}

	again, held, err := locker.TryLock(ctx, name)
	if err != nil {
		t.Fatalf("second TryLock() error: %v", err)
	}
	if !held {
		t.Fatal("second TryLock() did not take a lock that had been released")
	}
	if err := again.Release(ctx); err != nil {
		t.Errorf("Release() error: %v", err)
	}
}

func TestAdvisoryLockExcludesASecondInstance(t *testing.T) {
	t.Parallel()

	// Two pools, as two pods would hold: the lock is session-scoped, so
	// nothing short of a second connection can contend for it.
	mine, theirs := lockPool(t), lockPool(t)
	ctx := context.Background()
	name := jobName("contended")

	lock, held, err := scheduler.NewAdvisoryLocker(mine, scheduler.LockerConfig{}).TryLock(ctx, name)
	if err != nil {
		t.Fatalf("first TryLock() error: %v", err)
	}
	if !held {
		t.Fatal("first TryLock() did not take a free lock")
	}

	other, held, err := scheduler.NewAdvisoryLocker(theirs, scheduler.LockerConfig{}).TryLock(ctx, name)
	if err != nil {
		t.Fatalf("second TryLock() error = %v, want nil: a lock held elsewhere is an outcome, not a failure", err)
	}
	if held {
		t.Error("second TryLock() took a lock the first instance holds")
		if err := other.Release(ctx); err != nil {
			t.Errorf("Release() error: %v", err)
		}
	}

	if err := lock.Release(ctx); err != nil {
		t.Fatalf("Release() error: %v", err)
	}

	// And once the first instance lets go, the second gets it.
	other, held, err = scheduler.NewAdvisoryLocker(theirs, scheduler.LockerConfig{}).TryLock(ctx, name)
	if err != nil {
		t.Fatalf("third TryLock() error: %v", err)
	}
	if !held {
		t.Fatal("the second instance did not get the lock the first released")
	}
	if err := other.Release(ctx); err != nil {
		t.Errorf("Release() error: %v", err)
	}
}

func TestDifferentJobsDoNotExcludeEachOther(t *testing.T) {
	t.Parallel()

	mine, theirs := lockPool(t), lockPool(t)
	ctx := context.Background()

	poll, held, err := scheduler.NewAdvisoryLocker(mine, scheduler.LockerConfig{}).TryLock(ctx, jobName("network-poll"))
	if err != nil || !held {
		t.Fatalf("TryLock(network-poll) = held %v, error %v; want it taken", held, err)
	}
	t.Cleanup(func() { _ = poll.Release(ctx) })

	zeroSum, held, err := scheduler.NewAdvisoryLocker(theirs, scheduler.LockerConfig{}).TryLock(ctx, jobName("ledger-zero-sum"))
	if err != nil {
		t.Fatalf("TryLock(ledger-zero-sum) error: %v", err)
	}
	if !held {
		t.Fatal("a second job could not run while an unrelated one held its own lock")
	}
	if err := zeroSum.Release(ctx); err != nil {
		t.Errorf("Release() error: %v", err)
	}
}

func TestOnlyOneInstanceRunsAJobPerTick(t *testing.T) {
	t.Parallel()

	mine, theirs := lockPool(t), lockPool(t)
	name := jobName("per-tick")

	// Three ticks, because one proves exclusion and the rest prove the lock
	// is handed back between ticks: a leak would show as the first
	// instance losing its own job from tick two onwards.
	for tick := 1; tick <= 3; tick++ {
		entered := make(chan struct{})
		proceed := make(chan struct{})
		var ranHere, ranThere atomic.Bool

		first := newInstance(t, mine, name, func(context.Context) error {
			close(entered)
			<-proceed
			ranHere.Store(true)
			return nil
		})
		second := newInstance(t, theirs, name, func(context.Context) error {
			ranThere.Store(true)
			return nil
		})

		type outcome struct {
			ran bool
			err error
		}
		done := make(chan outcome, 1)
		go func() {
			ran, err := first.RunOnce(context.Background(), name)
			done <- outcome{ran: ran, err: err}
		}()

		// Wait until the first instance is provably inside the job, holding
		// the lock: the contention is then real, not a race the test hopes
		// to win.
		select {
		case <-entered:
		case <-time.After(30 * time.Second):
			t.Fatalf("tick %d: the first instance never entered its job", tick)
		}

		ran, err := second.RunOnce(context.Background(), name)
		if err != nil {
			t.Fatalf("tick %d: the second instance's RunOnce() error: %v", tick, err)
		}
		if ran {
			t.Errorf("tick %d: the second instance ran the job while the first held its lock", tick)
		}
		if ranThere.Load() {
			t.Errorf("tick %d: the second instance executed the job body; the lock did not exclude it", tick)
		}

		close(proceed)
		got := <-done
		if got.err != nil {
			t.Fatalf("tick %d: the first instance's RunOnce() error: %v", tick, got.err)
		}
		if !got.ran || !ranHere.Load() {
			t.Errorf("tick %d: the first instance did not run the job it held the lock for", tick)
		}
	}
}

func TestTwoRunningInstancesNeverOverlapAJob(t *testing.T) {
	t.Parallel()

	mine, theirs := lockPool(t), lockPool(t)
	name := jobName("overlap")

	var (
		peak scheduler.ConcurrencyPeak
		runs atomic.Int64
	)
	job := func(context.Context) error {
		defer peak.Enter()()
		runs.Add(1)
		time.Sleep(20 * time.Millisecond)
		return nil
	}

	// Both instances on their own schedule, ticking far faster than the job
	// takes, which is exactly when two pods would collide.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	for _, pool := range []*pgxpool.Pool{mine, theirs} {
		s := scheduler.New(slog.New(slog.DiscardHandler), scheduler.NewAdvisoryLocker(pool, scheduler.LockerConfig{}), scheduler.Config{})
		if err := s.Register(scheduler.Job{Name: name, Interval: time.Millisecond, Run: job}); err != nil {
			t.Fatalf("Register() error: %v", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Run(ctx); err != nil {
				t.Errorf("Run() error: %v", err)
			}
		}()
	}

	deadline := time.Now().Add(30 * time.Second)
	for runs.Load() < 4 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	wg.Wait()

	if got := runs.Load(); got < 4 {
		t.Fatalf("the job ran %d times across both instances, want at least 4", got)
	}
	if got := peak.Peak(); got != 1 {
		t.Errorf("maximum concurrent runs across two instances = %d, want 1", got)
	}
}

func TestAPanickingJobGivesItsAdvisoryLockBack(t *testing.T) {
	t.Parallel()

	mine, theirs := lockPool(t), lockPool(t)
	name := jobName("panicking")

	first := newInstance(t, mine, name, func(context.Context) error {
		panic("the job exploded")
	})
	ran, err := first.RunOnce(context.Background(), name)
	if !ran {
		t.Fatal("RunOnce() of a panicking job reported that it did not run")
	}
	var panicked *scheduler.PanicError
	if !errors.As(err, &panicked) {
		t.Fatalf("RunOnce() error = %v (%T), want a *scheduler.PanicError", err, err)
	}

	// The assertion that matters: a leaked lock would silently stop this
	// job on every instance for the lifetime of the one that panicked.
	var ranThere atomic.Bool
	second := newInstance(t, theirs, name, func(context.Context) error {
		ranThere.Store(true)
		return nil
	})
	ran, err = second.RunOnce(context.Background(), name)
	if err != nil {
		t.Fatalf("the second instance's RunOnce() error: %v", err)
	}
	if !ran || !ranThere.Load() {
		t.Error("the lock was still held after a job panicked; a panic must not strand it")
	}
}

func TestATimedOutRunGivesItsAdvisoryLockBack(t *testing.T) {
	t.Parallel()

	mine, theirs := lockPool(t), lockPool(t)
	name := jobName("timed-out")

	wedging := scheduler.New(slog.New(slog.DiscardHandler), scheduler.NewAdvisoryLocker(mine, scheduler.LockerConfig{}), scheduler.Config{})
	if err := wedging.Register(scheduler.Job{
		Name:     name,
		Interval: time.Hour,
		Timeout:  20 * time.Millisecond,
		Run: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}); err != nil {
		t.Fatalf("Register() error: %v", err)
	}
	if _, err := wedging.RunOnce(context.Background(), name); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunOnce() of a wedged job = %v, want context.DeadlineExceeded", err)
	}

	var ranThere atomic.Bool
	second := newInstance(t, theirs, name, func(context.Context) error {
		ranThere.Store(true)
		return nil
	})
	ran, err := second.RunOnce(context.Background(), name)
	if err != nil {
		t.Fatalf("the second instance's RunOnce() error: %v", err)
	}
	if !ran || !ranThere.Load() {
		t.Error("the lock was still held after a run timed out; a timeout must not strand it")
	}
}

func TestACancelledRunGivesItsAdvisoryLockBack(t *testing.T) {
	t.Parallel()

	mine, theirs := lockPool(t), lockPool(t)
	name := jobName("cancelled")

	entered := make(chan struct{})
	first := newInstance(t, mine, name, func(ctx context.Context) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		// The release runs on a context detached from this one, so a
		// shutdown mid-run must still hand the lock back.
		_, _ = first.RunOnce(ctx, name)
	}()

	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the first instance never entered its job")
	}
	cancel()
	<-done

	var ranThere atomic.Bool
	second := newInstance(t, theirs, name, func(context.Context) error {
		ranThere.Store(true)
		return nil
	})
	ran, err := second.RunOnce(context.Background(), name)
	if err != nil {
		t.Fatalf("the second instance's RunOnce() error: %v", err)
	}
	if !ran || !ranThere.Load() {
		t.Error("the lock was still held after a cancelled run; cancellation must not strand it")
	}
}

func TestAReleaseThatCannotReachTheDatabaseStillFreesTheLock(t *testing.T) {
	t.Parallel()

	mine, theirs := lockPool(t), lockPool(t)
	name := jobName("unreleasable")

	lock, held, err := scheduler.NewAdvisoryLocker(mine, scheduler.LockerConfig{}).TryLock(context.Background(), name)
	if err != nil || !held {
		t.Fatalf("TryLock() = held %v, error %v; want it taken", held, err)
	}

	// A release with no time left is the shutdown case: the unlock cannot
	// be sent. The connection is then destroyed rather than pooled, and
	// Postgres frees a session's advisory locks when its backend dies - so
	// the lock still ends up free, which is the whole point of destroying
	// it. A pooled-but-still-locked session would block this job on this
	// instance until the process ended.
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := lock.Release(expired); err == nil {
		t.Error("Release() with an expired context reported success")
	}

	locker := scheduler.NewAdvisoryLocker(theirs, scheduler.LockerConfig{})
	deadline := time.Now().Add(30 * time.Second)
	for {
		other, held, err := locker.TryLock(context.Background(), name)
		if err != nil {
			t.Fatalf("TryLock() error: %v", err)
		}
		if held {
			if err := other.Release(context.Background()); err != nil {
				t.Errorf("Release() error: %v", err)
			}
			return
		}
		if time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		t.Fatal("the lock was never freed after its connection was destroyed")
	}
}
