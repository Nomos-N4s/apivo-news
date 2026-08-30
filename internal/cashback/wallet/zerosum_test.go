package wallet_test

// The continuous C-1 check (T046), held to the same standard the platform
// drills hold the view underneath it: a check that has never been shown an
// imbalance proves nothing by finding none. So these tests build a ledger
// the check can see - a stand-in schema with the balance columns the view
// reads, named through the cashback.ledger_schema setting inside a
// rolled-back transaction, exactly the way
// internal/platform/db/cashback_provenance_test.go builds its own - and
// require the job to report the imbalance, to stay quiet over a balanced
// ledger while still saying what it verified, and to report the view
// RAISING - by SQLSTATE, the database's own refusal - as a failure of the
// check rather than as silence. The view itself is the platform drills'
// and T031's subject; this suite is about the job that runs it.
//
// Observability is asserted the way the scheduler's own suite asserts it:
// the check logs through slog, the tests capture the JSON records and read
// the structured attributes back. Amounts are compared as integer minor
// units (C-6); the decoder keeps JSON numbers as tokens so no float ever
// stands in for money, even in an assertion.
//
// They run against a real Postgres, keyed on DATABASE_URL like every suite
// beside them, on the pool conformance_test.go's TestMain opens.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
	"github.com/Nomos-N4s/apivo-news/internal/platform/logging"
	"github.com/Nomos-N4s/apivo-news/internal/platform/scheduler"
)

// codeRaiseException is the SQLSTATE of the RAISE 0016/0020 designed for a
// blinded check, the same constant the platform drills assert by. The code
// matters: any wrapped error could contain the right words, but only the
// database refusing carries its own SQLSTATE.
const codeRaiseException = "P0001"

// zeroSumTx opens a transaction on the shared pool that is always rolled
// back, so no stand-in ledger a test builds survives it, and skips - in
// the same words the sibling suites use - when no database is available.
func zeroSumTx(t *testing.T) pgx.Tx {
	t.Helper()
	if conformPool == nil {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set DATABASE_URL to exercise the zero-sum check")
	}
	tx, err := conformPool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

// standInSchemaName names a schema no other test can collide with. Naming
// is separate from pinning because the two matter separately: a pinned
// name nobody built is one blindness case, and a built name nobody pinned
// is how the wired-schema tests prove the check finds it on its own.
func standInSchemaName(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("naming the stand-in schema: %v", err)
	}
	return "zerosum_standin_" + hex.EncodeToString(raw)
}

// pinZeroSumSchema points the C-1 check at a schema of this test's own,
// transaction-locally, so the redirect cannot follow the connection back
// into the pool. The schema is named, not created: whether to create it -
// and what to put in it - is each test's whole point.
func pinZeroSumSchema(t *testing.T, tx pgx.Tx) string {
	t.Helper()
	schema := standInSchemaName(t)
	if _, err := tx.Exec(context.Background(),
		`select set_config('cashback.ledger_schema', $1, true)`, schema); err != nil {
		t.Fatalf("pointing the C-1 check at the stand-in schema: %v", err)
	}
	return schema
}

// standInLedger creates the named schema with the balance columns the
// view reads and the given (balance, currency) rows - the same stand-in
// the platform drills build for the view itself.
func standInLedger(t *testing.T, tx pgx.Tx, schema string, rows string) {
	t.Helper()
	ctx := context.Background()
	ident := pgx.Identifier{schema}.Sanitize()
	stmts := []string{
		"create schema " + ident,
		"create table " + ident + ".balances (balance bigint not null, currency text not null)",
	}
	// An empty rows string builds an empty ledger: a schema the check can
	// resolve and read, holding nothing to sum. That is the vacuous case,
	// and building it explicitly is what keeps the vacuous test honest on
	// a database that also carries a real ledger.
	if rows != "" {
		stmts = append(stmts, "insert into "+ident+".balances (balance, currency) values "+rows)
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			t.Fatalf("building the stand-in ledger (%s): %v", stmt, err)
		}
	}
}

// zeroSumCheck builds a check over tx with a capturing JSON logger,
// returning the check and the buffer its records land in. No schema is
// wired: the check reads whatever the transaction's own setting resolves,
// which is what the deployed check does under every driver but the
// Postgres exit route.
func zeroSumCheck(tx pgx.Tx) (*wallet.ZeroSumCheck, *strings.Builder) {
	var out strings.Builder
	log := logging.New(&out, slog.LevelDebug, config.EnvProd)
	return wallet.NewZeroSumCheck(log, tx, ""), &out
}

// zeroSumRecords parses the captured JSON log lines. Numbers stay
// json.Number: an assertion about a money delta must not pass through a
// float even here.
func zeroSumRecords(t *testing.T, out string) []map[string]any {
	t.Helper()
	var records []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		var record map[string]any
		if err := dec.Decode(&record); err != nil {
			t.Fatalf("log line %q is not one JSON record: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

// violationRecords returns the check's own violation records, keyed by
// their currency attribute, requiring each to be at ERROR and to carry its
// delta as net_minor. It selects on the message rather than on the level,
// because the scheduler adds an ERROR record of its own - "job failed",
// with no currency - beside the check's when a run is driven through it.
func violationRecords(t *testing.T, out string) map[string]json.Number {
	t.Helper()
	found := map[string]json.Number{}
	for _, record := range zeroSumRecords(t, out) {
		msg, ok := record["msg"].(string)
		if !ok || !strings.HasPrefix(msg, "C-1 violated") {
			continue
		}
		if record["level"] != "ERROR" {
			t.Fatalf("a violation record is at level %v, want ERROR: %v", record["level"], record)
		}
		currency, ok := record["currency"].(string)
		if !ok {
			t.Fatalf("a violation record carries no currency attribute: %v", record)
		}
		net, ok := record["net_minor"].(json.Number)
		if !ok {
			t.Fatalf("the violation record for %s carries no net_minor: %v", currency, record)
		}
		found[currency] = net
	}
	return found
}

// TestZeroSumCheckReportsAnImbalance is the test that stops the job being
// decorative, exactly as the platform drill stops the view being so: a
// ledger deliberately short 50 GBP, and the run must say so - at ERROR,
// with the currency and the integer delta as structured attributes, and
// with an error wrapping ErrOutOfBalance so the scheduler records the run
// as failed. The balanced EUR beside it must NOT be reported: a check that
// cries about clean currencies teaches people to stop reading it.
func TestZeroSumCheckReportsAnImbalance(t *testing.T) {
	t.Parallel()
	tx := zeroSumTx(t)
	schema := pinZeroSumSchema(t, tx)
	standInLedger(t, tx, schema, `(100, 'EUR'), (-100, 'EUR'), (50, 'GBP')`)

	check, out := zeroSumCheck(tx)
	err := check.Run(context.Background())
	if !errors.Is(err, wallet.ErrOutOfBalance) {
		t.Fatalf("Run() over a ledger short 50 GBP returned %v, want ErrOutOfBalance", err)
	}
	if !strings.Contains(err.Error(), "GBP +50") {
		t.Errorf("the error names %q, want the per-currency delta \"GBP +50\"", err)
	}

	broken := violationRecords(t, out.String())
	if len(broken) != 1 {
		t.Fatalf("the run reported %d currencies (%v), want exactly the broken one", len(broken), broken)
	}
	if got := broken["GBP"]; got.String() != "50" {
		t.Fatalf("the GBP violation record carries net_minor = %v, want 50", got)
	}
}

// TestZeroSumCheckKeepsReportingAStandingImbalance holds the job to the
// "continuously" in SC-003: an imbalance is an incident for as long as it
// stands, so the run after the one that found it must say so again, not
// treat it as old news. One log line that scrolled away is how an incident
// becomes a surprise.
func TestZeroSumCheckKeepsReportingAStandingImbalance(t *testing.T) {
	t.Parallel()
	tx := zeroSumTx(t)
	schema := pinZeroSumSchema(t, tx)
	standInLedger(t, tx, schema, `(50, 'GBP')`)

	check, out := zeroSumCheck(tx)
	for run := 1; run <= 2; run++ {
		out.Reset()
		if err := check.Run(context.Background()); !errors.Is(err, wallet.ErrOutOfBalance) {
			t.Fatalf("run %d returned %v, want ErrOutOfBalance while the imbalance stands", run, err)
		}
		if broken := violationRecords(t, out.String()); broken["GBP"].String() != "50" {
			t.Fatalf("run %d reported %v, want GBP 50 again: a standing incident must be restated every tick", run, broken)
		}
	}
}

// TestZeroSumCheckPinsTheWiredLedgerSchema is the exit-route case (0022):
// under LEDGER_DRIVER=postgres the composition root knows a co-located
// ledger lives in a schema 0020's default would never resolve, and a
// check left on that default would pass vacuously over the one ledger it
// is guaranteed to share a database with. So a check constructed with the
// schema's name must find an imbalance there with nothing set by anyone
// else - and must leave the setting untouched behind it, because the pin
// belongs to the run, not to the connection it borrowed.
func TestZeroSumCheckPinsTheWiredLedgerSchema(t *testing.T) {
	t.Parallel()
	tx := zeroSumTx(t)
	schema := standInSchemaName(t)
	standInLedger(t, tx, schema, `(50, 'GBP')`)

	var out strings.Builder
	log := logging.New(&out, slog.LevelDebug, config.EnvProd)
	check := wallet.NewZeroSumCheck(log, tx, schema)
	if err := check.Run(context.Background()); !errors.Is(err, wallet.ErrOutOfBalance) {
		t.Fatalf("Run() wired to a ledger short 50 GBP returned %v, want ErrOutOfBalance", err)
	}
	if broken := violationRecords(t, out.String()); broken["GBP"].String() != "50" {
		t.Fatalf("the wired run reported %v, want GBP 50", broken)
	}

	var setting string
	if err := tx.QueryRow(context.Background(),
		`select coalesce(current_setting('cashback.ledger_schema', true), '')`).Scan(&setting); err != nil {
		t.Fatalf("reading the setting back: %v", err)
	}
	if setting != "" {
		t.Fatalf("the schema pin leaked out of the run: cashback.ledger_schema is %q after Run returned", setting)
	}
}

// TestZeroSumCheckStaysQuietWhenTheLedgerNetsToZero pins the other half of
// the contract: a clean run returns nil and says nothing above DEBUG, in
// both shapes a clean deployment takes - a ledger whose every currency
// nets to zero, and a ledger with nothing in it, where vacuously true is
// the honest answer (0016). The two shapes must not share a sentence:
// "verified N currencies" and "summed nothing" are different facts, and a
// check that reports them identically cannot be told apart from one
// pointed at the wrong database.
func TestZeroSumCheckStaysQuietWhenTheLedgerNetsToZero(t *testing.T) {
	t.Parallel()

	t.Run("a balanced ledger", func(t *testing.T) {
		t.Parallel()
		tx := zeroSumTx(t)
		schema := pinZeroSumSchema(t, tx)
		standInLedger(t, tx, schema, `(100, 'EUR'), (-100, 'EUR')`)

		check, out := zeroSumCheck(tx)
		if err := check.Run(context.Background()); err != nil {
			t.Fatalf("Run() over a balanced ledger returned %v, want nil", err)
		}
		var verified bool
		for _, record := range zeroSumRecords(t, out.String()) {
			if record["level"] != "DEBUG" {
				t.Errorf("a clean run logged at %v: %v; anything above DEBUG here buries the run that matters", record["level"], record)
			}
			if record["msg"] == "the ledger nets to zero in every currency (C-1)" {
				verified = true
				if got, ok := record["currencies"].(json.Number); !ok || got.String() != "1" {
					t.Errorf("the clean record vouches for %v currencies, want 1 (EUR): %v", record["currencies"], record)
				}
			}
			if msg, _ := record["msg"].(string); strings.Contains(msg, "summed no currencies") {
				t.Errorf("a run that verified a real ledger reported itself vacuous: %v", record)
			}
		}
		if !verified {
			t.Fatalf("no record says the ledger was verified clean; output: %s", out.String())
		}
	})

	t.Run("an empty ledger", func(t *testing.T) {
		t.Parallel()
		tx := zeroSumTx(t)
		// A stand-in schema the check resolves and reads, holding no
		// balances at all. This is deliberately NOT "no schema anywhere":
		// the first spelling of this test relied on the database carrying
		// no ledger, and CI's shared database carries a populated Blnk
		// schema - so the check truthfully verified one currency and the
		// test failed on its own environmental assumption. An empty
		// ledger reaches the same vacuous branch on any database.
		schema := pinZeroSumSchema(t, tx)
		standInLedger(t, tx, schema, "")
		check, out := zeroSumCheck(tx)
		if err := check.Run(context.Background()); err != nil {
			t.Fatalf("Run() over an empty ledger returned %v, want nil (vacuously true is honest here)", err)
		}
		var vacuous bool
		for _, record := range zeroSumRecords(t, out.String()) {
			if record["level"] != "DEBUG" {
				t.Errorf("a vacuous run logged at %v: %v", record["level"], record)
			}
			if msg, _ := record["msg"].(string); strings.Contains(msg, "summed no currencies") {
				vacuous = true
			}
			if record["msg"] == "the ledger nets to zero in every currency (C-1)" {
				t.Errorf("a run that summed nothing claimed to have verified the ledger: %v", record)
			}
		}
		if !vacuous {
			t.Fatalf("no record says the run was vacuous; output: %s", out.String())
		}
	})
}

// TestZeroSumCheckReportsItsOwnBlindnessAsAFailure is the case the task of
// running the check must never soften: when the view RAISES - a named
// schema that is not there, a schema present whose balances cannot be read
// - the check could not see the postings it exists to sum, and the run
// must fail as the CHECK failing. It must not return nil, it must not
// return ErrOutOfBalance - "the check is blind" and "the ledger is wrong"
// demand different responses, and reporting either as the other misdirects
// the person answering the page - and the failure must carry the
// database's own refusal by SQLSTATE: the designed 0016/0020 RAISE, not
// any error whose words happen to fit.
func TestZeroSumCheckReportsItsOwnBlindnessAsAFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// build arranges the blindness and returns the schema to wire into
		// the check's constructor - "" for the cases where the pin comes
		// from the transaction, the way a test or an ALTER DATABASE sets
		// it, rather than from the composition root.
		build func(t *testing.T, tx pgx.Tx) string
	}{
		{
			name: "a named schema that is not there",
			build: func(t *testing.T, tx pgx.Tx) string {
				// Named, deliberately never created: the claim about where
				// the ledger is is false, and 0020 makes that a raise.
				pinZeroSumSchema(t, tx)
				return ""
			},
		},
		{
			name: "a wired schema that is not there",
			build: func(t *testing.T, _ pgx.Tx) string {
				// The composition root's own claim can be the false one: a
				// check wired for the exit route against a database that
				// never ran 0022 must fail, not report that nothing summed
				// to nothing.
				return standInSchemaName(t)
			},
		},
		{
			name: "a ledger present but unreadable",
			build: func(t *testing.T, tx pgx.Tx) string {
				// The schema exists but holds no balances relation - the
				// present-but-unreadable shape 0016 refuses to report as
				// zero rows.
				schema := pinZeroSumSchema(t, tx)
				if _, err := tx.Exec(context.Background(),
					"create schema "+pgx.Identifier{schema}.Sanitize()); err != nil {
					t.Fatalf("creating the empty ledger schema: %v", err)
				}
				return ""
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tx := zeroSumTx(t)
			wired := tt.build(t, tx)

			var out strings.Builder
			log := logging.New(&out, slog.LevelDebug, config.EnvProd)
			check := wallet.NewZeroSumCheck(log, tx, wired)
			err := check.Run(context.Background())
			if err == nil {
				t.Fatal("Run() returned nil where the view raises: the check reported blindness as a clean ledger")
			}
			if errors.Is(err, wallet.ErrOutOfBalance) {
				t.Fatalf("Run() returned ErrOutOfBalance (%v) where the check itself failed: blindness reported as an imbalance", err)
			}
			if !strings.Contains(err.Error(), "failure of the check") {
				t.Errorf("the error %q does not name itself a failure of the check", err)
			}
			// The database is what refused, and the error must still carry
			// its refusal: the RAISE 0016/0020 designed, by SQLSTATE. A
			// wrapper with the right words around a connection failure or
			// an aborted transaction would pass every assertion above.
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) {
				t.Fatalf("the failure does not carry the database's own refusal: got %T: %v", err, err)
			}
			if pgErr.Code != codeRaiseException {
				t.Fatalf("want SQLSTATE %s (the 0016/0020 RAISE), got %s: %s", codeRaiseException, pgErr.Code, pgErr.Message)
			}
		})
	}
}

// TestZeroSumCheckRunsAsAScheduledJob drives one tick through the real
// scheduler and the real advisory locker - everything main.go wires except
// main.go itself. Register owns the job's identity, so this is also where
// the name is pinned: the fleet-wide lock excludes two instances only
// while both registrations produce it.
func TestZeroSumCheckRunsAsAScheduledJob(t *testing.T) {
	t.Parallel()
	tx := zeroSumTx(t)
	schema := pinZeroSumSchema(t, tx)
	standInLedger(t, tx, schema, `(50, 'GBP')`)

	var out strings.Builder
	log := logging.New(&out, slog.LevelDebug, config.EnvProd)
	jobs := scheduler.New(log, scheduler.NewAdvisoryLocker(conformPool, scheduler.LockerConfig{}), scheduler.Config{})
	if err := wallet.NewZeroSumCheck(log, tx, "").Register(jobs); err != nil {
		t.Fatalf("Register() error: %v", err)
	}

	ran, err := jobs.RunOnce(context.Background(), wallet.ZeroSumJobName)
	if !ran {
		t.Fatalf("RunOnce(%q) did not run the job here: %v", wallet.ZeroSumJobName, err)
	}
	if !errors.Is(err, wallet.ErrOutOfBalance) {
		t.Fatalf("the scheduled run returned %v, want ErrOutOfBalance", err)
	}

	// The imbalance is loud on both channels: the check's own record with
	// the delta, and the scheduler recording the run as failed under the
	// job's name.
	if broken := violationRecords(t, out.String()); broken["GBP"].String() != "50" {
		t.Fatalf("the scheduled run reported %v, want GBP 50", broken)
	}
	var failed bool
	for _, record := range zeroSumRecords(t, out.String()) {
		if record["msg"] == "job failed" && record["job"] == wallet.ZeroSumJobName {
			failed = true
		}
	}
	if !failed {
		t.Fatalf("the scheduler never recorded a failed %q run; output: %s", wallet.ZeroSumJobName, out.String())
	}
}
