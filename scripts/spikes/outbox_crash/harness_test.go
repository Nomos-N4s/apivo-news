// Package outboxcrash_test is spike S2 (ADR-0002, task T007).
//
// Question: can an Apivo transaction and a Blnk transfer be made reliably
// consistent through the outbox with a shared idempotency key — including
// when the process is killed between the two?
//
// The answer decides a documented fallback. ADR-0002 says a failing S2 is a
// founder decision, not something to work around, so this spike reports what
// it observes rather than adjusting the design until it passes.
//
// The shape under test is the one data-model.md specifies:
//
//  1. One Postgres transaction writes the domain row AND an outbox row
//     carrying a deterministic idempotency key, and commits. Nothing has
//     reached the ledger yet.
//  2. A dispatcher reads undispatched outbox rows and posts a transfer whose
//     ledger reference IS that idempotency key, then marks the row.
//
// There are exactly two windows a crash can land in, and this file kills a
// real process in each of them:
//
//	after the commit, before the ledger call  -> nothing was posted; the
//	  outbox row is the durable instruction to post it, and replay must
//	  produce exactly one transfer.
//	after the ledger call, before the mark    -> the transfer exists but the
//	  outbox row still says "pending", so recovery WILL replay it; the
//	  ledger must absorb the replay without moving money twice.
//
// The crash is not a mocked error. The test re-executes its own binary with
// S2_WORKER_MODE set, that child does the work and calls os.Exit, and the
// parent then asserts on the state the dead process left behind. Nothing is
// flushed, nothing deferred runs, no rollback is attempted — which is what a
// kill -9 in production looks like.
//
// This file is test-only, so it contributes no statements to the coverage
// gate and nothing here is linked into the binary. It needs a real Postgres
// and a real ledger, so it skips on the founder's machine while Docker
// Desktop is unavailable and runs in the cashback CI job, which is the
// verification of record.
package outboxcrash_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"os/exec"
	"testing"
	"time"

	blnkgo "github.com/blnkfinance/blnk-go"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// workerModeEnv turns this test binary into the short-lived worker the
	// parent kills. TestMain checks it before any test runs.
	workerModeEnv = "S2_WORKER_MODE"
	workerKeyEnv  = "S2_IDEMPOTENCY_KEY"
	workerSrcEnv  = "S2_SOURCE_BALANCE"
	workerDstEnv  = "S2_DESTINATION_BALANCE"

	// modeCommitThenCrash writes the domain row and the outbox row in one
	// transaction, commits, and dies before touching the ledger.
	modeCommitThenCrash = "commit-then-crash"
	// modePostThenCrash posts the transfer and dies before marking the
	// outbox row, so recovery is guaranteed to replay it.
	modePostThenCrash = "post-then-crash"

	// crashExit is the status the worker dies with. It is distinctive so a
	// worker that failed for some other reason cannot be mistaken for one
	// that crashed where the test wanted it to.
	crashExit = 9

	// schema holds the spike's own tables. It is dropped at the end of the
	// run: a spike that leaves furniture behind in a shared database is a
	// spike that breaks the next one.
	schema = "spike_s2"

	currency = "EUR"
	// precision is minor units per major unit. Money is integer minor units
	// everywhere in Apivo (C-6); the ledger is told the scale so it stores
	// the same integer.
	precision = 100
	// transferMinor is 25.00 EUR, in minor units. It is sent to the ledger
	// as precise_amount and never as a float - see postTransfer.
	transferMinor = 2500
)

// TestMain intercepts the worker invocation before the testing framework
// parses a single flag. A process that is about to be killed on purpose
// should not also be a test runner.
func TestMain(m *testing.M) {
	if mode := os.Getenv(workerModeEnv); mode != "" {
		runWorker(mode)
		return // unreachable: runWorker always exits
	}
	os.Exit(m.Run())
}

// runWorker is the child process. Every path out of it is os.Exit, and the
// successful paths exit with crashExit, because "succeeded and then died" is
// the only outcome the parent is interested in.
func runWorker(mode string) {
	ctx := context.Background()
	key := os.Getenv(workerKeyEnv)
	source := os.Getenv(workerSrcEnv)
	destination := os.Getenv(workerDstEnv)
	if key == "" || source == "" || destination == "" {
		fmt.Fprintln(os.Stderr, "worker: key, source and destination are required")
		os.Exit(2)
	}

	switch mode {
	case modeCommitThenCrash:
		pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "worker: connecting:", err)
			os.Exit(2)
		}
		if err := writeEntryAndOutbox(ctx, pool, key, source, destination); err != nil {
			fmt.Fprintln(os.Stderr, "worker: writing the outbox:", err)
			os.Exit(2)
		}
		// The commit has returned. The ledger knows nothing. Die here,
		// with no flush and no deferred close, exactly as a kill would.
		os.Exit(crashExit)

	case modePostThenCrash:
		client, err := newLedgerClient()
		if err != nil {
			fmt.Fprintln(os.Stderr, "worker: building the ledger client:", err)
			os.Exit(2)
		}
		if _, err := postTransfer(client, key, source, destination); err != nil {
			fmt.Fprintln(os.Stderr, "worker: posting the transfer:", err)
			os.Exit(2)
		}
		// The ledger has the transfer. The outbox row still says pending,
		// so recovery is guaranteed to try again. Die.
		os.Exit(crashExit)

	default:
		fmt.Fprintln(os.Stderr, "worker: unknown mode", mode)
		os.Exit(2)
	}
}

// runWorkerProcess re-executes this test binary as the worker and requires it
// to have died where it was told to.
func runWorkerProcess(t *testing.T, mode, key, source, destination string) {
	t.Helper()

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		workerModeEnv+"="+mode,
		workerKeyEnv+"="+key,
		workerSrcEnv+"="+source,
		workerDstEnv+"="+destination,
	)
	output, err := cmd.CombinedOutput()
	t.Logf("worker(%s) output:\n%s", mode, output)

	if err == nil {
		t.Fatalf("worker(%s) exited cleanly; it was supposed to crash", mode)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("worker(%s) failed to run: %v", mode, err)
	}
	if code := exitErr.ExitCode(); code != crashExit {
		t.Fatalf("worker(%s) exited %d, want %d - it failed before reaching the crash point", mode, code, crashExit)
	}
}

// requireStack skips unless both halves of the stack are present. This is the
// same posture the invariant suites take with DATABASE_URL: expected to skip
// locally, never skipped in the cashback CI job.
func requireStack(t *testing.T) {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL is unset: no Postgres (expected without Docker)")
	}
	if os.Getenv("BLNK_URL") == "" {
		t.Skip("BLNK_URL is unset: no ledger (expected without Docker)")
	}
}

func newLedgerClient() (*blnkgo.Client, error) {
	raw := os.Getenv("BLNK_URL")
	base, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("BLNK_URL is not a URL: %w", err)
	}
	key := os.Getenv("BLNK_SECRET_KEY")
	return blnkgo.NewClient(base, &key), nil
}

// postTransfer is the one place a transfer is created, shared by the worker
// and by the parent's dispatcher, so a replay is byte-identical to the
// original call. The reference IS the outbox row's idempotency key: one key
// spans the Apivo transaction and the ledger transfer, which is the whole
// claim S2 tests.
func postTransfer(client *blnkgo.Client, key, source, destination string) (*blnkgo.Transaction, error) {
	txn, _, err := client.Transaction.Create(blnkgo.CreateTransactionRequest{
		ParentTransaction: blnkgo.ParentTransaction{
			Reference: key,
			Currency:  currency,
			Precision: precision,
			// PreciseAmount ONLY. The ledger rejects a request carrying
			// both amount and precise_amount ("either amount or
			// precise_amount should be provided, not both"), and the
			// integer one is the only one Apivo may send anyway: money is
			// integer minor units everywhere, and no float crosses this
			// boundary (C-6). The wallet adapter (T043) inherits the
			// same rule.
			PreciseAmount: big.NewInt(transferMinor),
			Source:        source,
			Destination:   destination,
			Description:   "spike S2 outbox transfer",
			// Inline rather than queued: the spike asserts on the state
			// immediately after the call, and a queued transfer would
			// make every assertion a race against a worker this job does
			// not run.
			SkipQueue: true,
		},
		// The source starts empty. Cashback's real source is a house
		// account funded by the network's commission; here the point is
		// the idempotency key, not the funding.
		AllowOverdraft: true,
	})
	if err != nil {
		return nil, err
	}
	return txn, nil
}

// newKey mints an idempotency key. In production it is derived
// deterministically from the withdrawal request (C-5); here it only has to
// be unique per test so one run cannot see another's transfers.
func newKey(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("minting an idempotency key: %v", err)
	}
	return "spike-s2-" + hex.EncodeToString(buf)
}

func connect(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatalf("connecting to Postgres: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// ensureSchema creates the spike's own tables. They are deliberately NOT the
// production outbox: `public.domain_event` grows its idempotency key in
// migration 0018, which is another task's work, and a spike that depended on
// it could not run until that landed.
func ensureSchema(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	statements := []string{
		`CREATE SCHEMA IF NOT EXISTS ` + schema,
		`CREATE TABLE IF NOT EXISTS ` + schema + `.entry (
			id            text PRIMARY KEY,
			amount_minor  bigint NOT NULL,
			currency      char(3) NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS ` + schema + `.outbox (
			idempotency_key     text PRIMARY KEY,
			entry_id            text NOT NULL REFERENCES ` + schema + `.entry(id),
			source_balance      text NOT NULL,
			destination_balance text NOT NULL,
			created_at          timestamptz NOT NULL DEFAULT now(),
			dispatched_at       timestamptz
		)`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("preparing the spike schema: %v", err)
		}
	}
}

// writeEntryAndOutbox is step 1: the domain row and the instruction to move
// money, in ONE transaction. Either both exist or neither does, and that is
// the property the whole pattern rests on.
func writeEntryAndOutbox(ctx context.Context, pool *pgxpool.Pool, key, source, destination string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`INSERT INTO `+schema+`.entry (id, amount_minor, currency) VALUES ($1, $2, $3)`,
		key, int64(transferMinor), currency,
	); err != nil {
		return fmt.Errorf("inserting the entry: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO `+schema+`.outbox (idempotency_key, entry_id, source_balance, destination_balance)
		 VALUES ($1, $1, $2, $3)`,
		key, source, destination,
	); err != nil {
		return fmt.Errorf("inserting the outbox row: %w", err)
	}
	return tx.Commit(ctx)
}

// dispatch is step 2: the recovery path. It reads an undispatched outbox row,
// posts the transfer under that row's key, and only then marks the row. The
// order is the point - marking first would lose the instruction on a crash,
// which is the failure this whole design exists to prevent.
func dispatch(ctx context.Context, pool *pgxpool.Pool, client *blnkgo.Client, key string) error {
	var source, destination string
	err := pool.QueryRow(ctx,
		`SELECT source_balance, destination_balance FROM `+schema+`.outbox
		  WHERE idempotency_key = $1 AND dispatched_at IS NULL`, key,
	).Scan(&source, &destination)
	if err != nil {
		return fmt.Errorf("reading the pending outbox row: %w", err)
	}
	if _, err := postTransfer(client, key, source, destination); err != nil {
		return fmt.Errorf("posting the transfer: %w", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE `+schema+`.outbox SET dispatched_at = now() WHERE idempotency_key = $1`, key,
	); err != nil {
		return fmt.Errorf("marking the outbox row: %w", err)
	}
	return nil
}

// ledgerRowsFor counts the ledger's own rows for one reference, read straight
// out of Postgres rather than through the API. Apivo's role can do this
// because Blnk's schema is co-located in the same database (ADR-0002, spike
// S1), which is also what makes the C-1 zero-sum check one query.
func ledgerRowsFor(ctx context.Context, t *testing.T, pool *pgxpool.Pool, key string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM blnk.transactions WHERE reference = $1`, key,
	).Scan(&count); err != nil {
		t.Fatalf("counting ledger rows for %s: %v", key, err)
	}
	return count
}

// balanceOf reads a ledger balance as text, so the assertion does not depend
// on which numeric type the ledger's schema uses this version.
func balanceOf(ctx context.Context, t *testing.T, pool *pgxpool.Pool, balanceID string) string {
	t.Helper()
	var balance string
	if err := pool.QueryRow(ctx,
		`SELECT balance::text FROM blnk.balances WHERE balance_id = $1`, balanceID,
	).Scan(&balance); err != nil {
		t.Fatalf("reading balance %s: %v", balanceID, err)
	}
	return balance
}

// waitForLedgerRows polls until the ledger shows at least want rows for the
// reference, so a settled assertion is never a race. It returns the count it
// settled on.
func waitForLedgerRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, key string, want int) int {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	count := 0
	for time.Now().Before(deadline) {
		count = ledgerRowsFor(ctx, t, pool, key)
		if count >= want {
			return count
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("the ledger still shows %d rows for %s after 20s, want at least %d", count, key, want)
	return count
}

// settle gives the ledger a moment to do anything it was going to do, so an
// assertion that something did NOT happen is not merely early.
func settle() { time.Sleep(2 * time.Second) }

// newLedgerAccounts creates a ledger and two balances to move money between,
// once per test, so no test can see another's money.
func newLedgerAccounts(t *testing.T, client *blnkgo.Client) (source, destination string) {
	t.Helper()

	ledger, _, err := client.Ledger.Create(blnkgo.CreateLedgerRequest{Name: "apivo-spike-s2-" + newKey(t)})
	if err != nil {
		t.Fatalf("creating a ledger: %v", err)
	}
	newBalance := func(role string) string {
		balance, _, err := client.LedgerBalance.Create(blnkgo.CreateLedgerBalanceRequest{
			LedgerID: ledger.LedgerID,
			Currency: currency,
		})
		if err != nil {
			t.Fatalf("creating the %s balance: %v", role, err)
		}
		return balance.BalanceID
	}
	return newBalance("source"), newBalance("destination")
}
