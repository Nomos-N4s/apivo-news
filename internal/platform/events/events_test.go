package events_test

// Shared scaffolding for the outbox and dispatcher suites.
//
// The integration tests run against a real Postgres, keyed on
// DATABASE_URL exactly as the schema suites in internal/platform/db are.
// The pure validation, registration and checkpoint tests run without one.
//
// Two databases are in play. The outbox tests use the suite's own
// database inside transactions that are always rolled back, like every
// sibling suite - domain_event is append-only, so a committed test row
// would be there forever. The dispatcher tests need commits: their
// subject matter is events whose occurred_at actually advances between
// appends and whose delivery crosses transaction boundaries, so they run
// against a scratch database that is dropped and recreated per run.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	baseURL := os.Getenv("DATABASE_URL")
	if baseURL != "" {
		if err := db.Migrate(baseURL); err != nil {
			fmt.Fprintln(os.Stderr, "migrating test database:", err)
			os.Exit(1)
		}
		cfg, err := pgxpool.ParseConfig(baseURL)
		if err != nil {
			fmt.Fprintln(os.Stderr, "parsing test database URL:", err)
			os.Exit(1)
		}
		// Every subtest holds a transaction, and hence a connection, for
		// its whole run; size the pool so parallel subtests do not queue
		// behind their own connections (see internal/platform/db).
		if want := int32(runtime.GOMAXPROCS(0)) + 4; cfg.MaxConns < want {
			cfg.MaxConns = want
		}
		pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "connecting test database:", err)
			os.Exit(1)
		}
		testPool = pool
	}
	code := m.Run()
	if testPool != nil {
		testPool.Close()
	}
	if dispatchPool != nil {
		dispatchPool.Close()
	}
	os.Exit(code)
}

// beginTx opens a transaction that is always rolled back, keeping tests
// independent and the append-only stream clean.
func beginTx(t *testing.T) pgx.Tx {
	t.Helper()
	if testPool == nil {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the outbox")
	}
	tx, err := testPool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random: %v", err)
	}
	return hex.EncodeToString(b)
}

// The dispatcher suites share one scratch database, built on first use.
// The error is stored rather than reported inside the once, so every
// test that needs the database sees the same verdict.
var (
	dispatchOnce sync.Once
	dispatchPool *pgxpool.Pool
	dispatchErr  error
)

// dispatcherDB returns a pool on the shared scratch database, creating
// and migrating it on the first call. Tests keep to their own event
// types (randomSuffix), so the shared stream never couples them: a
// dispatcher passes over every type it has no handler for.
func dispatcherDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	baseURL := os.Getenv("DATABASE_URL")
	if baseURL == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the dispatcher")
	}
	dispatchOnce.Do(func() {
		dispatchPool, dispatchErr = newScratchPool(baseURL, "apivo_dispatcher")
	})
	if dispatchErr != nil {
		t.Fatalf("building the dispatcher scratch database: %v", dispatchErr)
	}
	return dispatchPool
}

// Every wait in these suites that ticks a dispatcher or a registry until
// something is true runs on a wall-clock budget rather than on a count of
// ticks, and tickWait is that budget.
//
// The reason is what such a wait is really waiting for. A checkpoint may
// only be saved over rows no append still in flight can land in front of,
// and pollStream decides that two ways: the oldest transaction open in
// THIS database, and pg_snapshot_xmin(pg_current_snapshot()), the oldest
// transaction id still running anywhere. The second bound is not scoped to
// a database and cannot be - transaction ids are cluster-wide - so one
// writing transaction left open on any database of the same server holds
// the horizon at its own id, and every row committed after it is judged
// unsettled until that transaction ends. Running the dispatcher suites on
// a scratch database of their own buys no insulation from it: a sibling
// package driving migrations up and down on a scratch database of its own
// pins this stream's checkpoint for as long as each of its transactions
// lasts.
//
// That is the production rule behaving exactly as designed. It waits, which
// is the safe direction, and it never skips. What it makes wrong is
// counting ticks: what one of these waits has to outlast is another
// backend's transaction, measured in seconds, while 500 millisecond-paced
// ticks is about a second and a half of wall clock - less than the
// four-second stretches the horizon has been measured stalling for while
// internal/platform/db drives its migration round trip on the same cluster.
// Every test that failed that way was waiting for a checkpoint to CATCH UP,
// which is liveness. The no-skip assertions are safety and were never in
// question, and none of them is on this budget.
//
// So the budget is time, and it is spent only where spending it means
// something:
//
//   - settleBudget is the plain wait. With the horizon clear a checkpoint
//     catches up within a tick or two, so fifteen seconds of it is a defect
//     rather than a slow machine.
//   - settleGrace is granted again and again whenever that budget runs out
//     while another backend still holds a transaction id. Those
//     transactions are short-lived and the horizon does advance repeatedly,
//     so a busy cluster costs the wait patience instead of a failure - and
//     the patience is only ever extended while the cluster is visibly the
//     reason.
//   - settleCeiling ends the wait whatever the cluster is doing, so a
//     transaction someone left open forever fails the test with a diagnosis
//     rather than running the package into its timeout.
//
// The shape is the fix. A count of fast ticks is what this was, and an
// intermittently red CI job is what that cost; anyone tempted to simplify
// it back into one should reproduce the failure first, which takes nothing
// more than an idle-in-transaction psql on an unrelated database.
const (
	settleBudget   = 15 * time.Second
	settleGrace    = 2 * time.Second
	settleCeiling  = 60 * time.Second
	settlePause    = time.Millisecond
	settleMaxPause = 25 * time.Millisecond
	// settleFastTicks is how many ticks are paced tightly before the pause
	// starts lengthening. A healthy wait ends in a handful of them; past
	// that the wait is plainly on something outside the test, and polling
	// harder only competes with whatever it is waiting for.
	settleFastTicks = 50
)

// tickWait paces one poll-driven wait and decides when to give up on it.
// The zero value is not usable; start one with newTickWait.
type tickWait struct {
	started  time.Time
	deadline time.Time
	ceiling  time.Time
	pause    time.Duration
	ticks    int
	gaveUp   string
}

// newTickWait starts a wait's budget.
func newTickWait() *tickWait {
	now := time.Now()
	return &tickWait{
		started:  now,
		deadline: now.Add(settleBudget),
		ceiling:  now.Add(settleCeiling),
		pause:    settlePause,
	}
}

// next pauses before the next tick and reports whether the wait may go on.
// When the budget is spent the cluster decides: while another backend holds
// a transaction id the horizon cannot advance and no checkpoint can settle,
// which is a neighbour rather than a defect, so the wait is granted more
// time up to the ceiling. Otherwise the wait is over and reason says which
// of the two it was.
func (w *tickWait) next(t *testing.T) bool {
	t.Helper()
	now := time.Now()
	if now.Before(w.deadline) {
		w.rest()
		return true
	}
	held := horizonHolders(t)
	if held != "" && now.Before(w.ceiling) {
		w.deadline = now.Add(settleGrace)
		w.rest()
		return true
	}
	waited := time.Since(w.started).Round(time.Millisecond)
	if held == "" {
		w.gaveUp = fmt.Sprintf("waited %s and no backend this role can see was holding a transaction id, so nothing outside this test was holding a checkpoint back", waited)
	} else {
		w.gaveUp = fmt.Sprintf("waited %s and the transaction-id horizon is still held by %s, which pins every checkpoint on this cluster", waited, held)
	}
	return false
}

// rest pauses between ticks, lengthening the pause once the tightly paced
// ticks are used up.
func (w *tickWait) rest() {
	time.Sleep(w.pause)
	w.ticks++
	if w.ticks > settleFastTicks && w.pause < settleMaxPause {
		if w.pause *= 2; w.pause > settleMaxPause {
			w.pause = settleMaxPause
		}
	}
}

// reason explains how the wait ended, for the failure message that reports
// it. It is empty until next has returned false.
func (w *tickWait) reason() string { return w.gaveUp }

// horizonHolders renders every client backend holding a transaction id at
// this instant - each one a reason pg_snapshot_xmin cannot advance, whatever
// database it happens to be connected to - and is empty when there are none.
// It is what tells a wait that ran out of time apart from a wait that was
// never going to finish.
//
// It sees the same sessions oldestOpenTransaction does: the reading role's
// own, and every role's where that role holds pg_read_all_stats. The suites
// all connect as one role, so a holder among them is always visible; a
// holder belonging to some other role on a shared cluster would not be, and
// the wait would then report a clear horizon it cannot actually see.
func horizonHolders(t *testing.T) string {
	t.Helper()
	var horizon, holders string
	err := dispatcherDB(t).QueryRow(context.Background(),
		`select pg_snapshot_xmin(pg_current_snapshot())::text,
		        coalesce(string_agg(datname || ' pid ' || pid || ' xid ' || backend_xid ||
		                            ' open ' || round(extract(epoch from clock_timestamp() - xact_start)::numeric, 3) || 's',
		                            '; ' order by xact_start), '')
		 from pg_stat_activity
		 where backend_type = 'client backend' and backend_xid is not null`).Scan(&horizon, &holders)
	if err != nil {
		return fmt.Sprintf("(the horizon could not be read: %v)", err)
	}
	if holders == "" {
		return ""
	}
	return holders + " (horizon at xid " + horizon + ")"
}

// newScratchPool drops, recreates and migrates a database beside the test
// one, and returns a pool on it.
func newScratchPool(baseURL, name string) (*pgxpool.Pool, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("DATABASE_URL is not a URL: %w", err)
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		return nil, fmt.Errorf("connecting: %w", err)
	}
	defer func() { _ = admin.Close(ctx) }()

	if _, err := admin.Exec(ctx, "drop database if exists "+name+" with (force)"); err != nil {
		return nil, fmt.Errorf("dropping scratch database: %w", err)
	}
	if _, err := admin.Exec(ctx, "create database "+name); err != nil {
		return nil, fmt.Errorf("creating scratch database: %w", err)
	}

	u.Path = "/" + name
	if err := db.Migrate(u.String()); err != nil {
		return nil, fmt.Errorf("migrating scratch database: %w", err)
	}
	return pgxpool.New(ctx, u.String())
}
