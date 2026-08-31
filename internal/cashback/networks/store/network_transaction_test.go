package store_test

// Exercises the generated networks store against the real, migrated schema
// (T052). The point is not coverage of generated code but proof that the
// insert's contract with 0012 holds where it is decided - in the database.
//
// Two of those decisions are invisible from Go and would be believed rather
// than known without this file. The digest is computed by a BEFORE INSERT
// trigger from the reported FACTS, and the insert deliberately does not name
// the column; and the trigger deliberately does not hash raw_payload,
// because networks stamp their own response timestamps and pagination
// metadata into a payload, which would make every re-report look like a
// change and every poll create a new row. Both are asserted here by
// observing the digest the database chose, not by reading the migration.
//
// Everything runs inside one transaction that is rolled back, so the suite
// leaves no rows behind, and every case runs inside a SAVEPOINT of its own
// within it. That is stricter than the catalogue store's suite, which keeps
// its cases apart by addressing distinct rows, and it has to be: half the
// cases here provoke a constraint violation on purpose, and a failed
// statement aborts the whole transaction it ran in. Without a savepoint per
// case the first expected refusal would take every later case down with it,
// and they would fail for a reason that has nothing to do with what they
// assert - which is exactly how this file failed the first time it ran.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// The store is wired against whatever can run its queries; both the pool
// and a transaction must satisfy that, checked here at compile time.
var (
	_ store.DBTX = (*pgxpool.Pool)(nil)
	_ store.DBTX = (pgx.Tx)(nil)
)

// SQLSTATE codes this file asserts on. Named, because a bare string in a
// comparison is a code nobody can look up from the failure message.
const (
	codeUniqueViolation = "23505"
	codeCheckViolation  = "23514"
)

// account seeds a network and a publisher account at it, returning the pair
// the insert needs. Each call makes its own network, so two cases can report
// the same external transaction id without meeting.
func account(ctx context.Context, t *testing.T, tx pgx.Tx) (networkID string, accountID pgtype.UUID) {
	t.Helper()

	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	// network.id is constrained to ^[a-z][a-z0-9_]*$, so the suffix is
	// lower-case hex behind a letter.
	networkID = "fixture_" + hex.EncodeToString(suffix)

	if _, err := tx.Exec(ctx, `
		insert into cashback.network (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_minute, active)
		values ($1, 'Conformance Network', 'clickref', 31, 360, true)`, networkID); err != nil {
		t.Fatalf("seeding the network: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		insert into cashback.network_account (network_id, external_publisher_id, credential_ref, active)
		values ($1, 'publisher-1', 'config:networks.fixture.credential', true)
		returning id`, networkID).Scan(&accountID); err != nil {
		t.Fatalf("seeding the publisher account: %v", err)
	}
	return networkID, accountID
}

// seedAccount makes an account in the given role, for the tests that need a
// member to credit or an operator to resolve.
func seedAccount(ctx context.Context, t *testing.T, tx pgx.Tx, role string) pgtype.UUID {
	t.Helper()
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	var id pgtype.UUID
	if err := tx.QueryRow(ctx, `
		insert into public.account (email, display_name, role)
		values ($1, 'Test Person', $2) returning id`,
		hex.EncodeToString(suffix)+"@example.test", role).Scan(&id); err != nil {
		t.Fatalf("seeding an account: %v", err)
	}
	return id
}

// report is one insert's worth of parameters, already valid. A case states
// only what it changes, so the mutation under test is visible at the call
// site rather than buried in a helper.
func report(networkID string, accountID pgtype.UUID) store.InsertNetworkTransactionParams {
	at := time.Date(2026, time.August, 3, 9, 15, 0, 0, time.UTC)
	return store.InsertNetworkTransactionParams{
		NetworkID:        networkID,
		NetworkAccountID: accountID,
		ExternalID:       "FIX-1001",
		ClickRef:         pgtype.Text{String: "Zml4dHVyZS1jbGljay0wMDAwMDAwMQ", Valid: true},
		StatusRaw:        "pending",
		Status:           "pending",
		SaleAmountMinor:  4999,
		CommissionMinor:  499,
		Currency:         "EUR",
		TransactedAt:     pgtype.Timestamptz{Time: at, Valid: true},
		RetrievedAt:      pgtype.Timestamptz{Time: at.Add(time.Hour), Valid: true},
		QueryWindowStart: pgtype.Timestamptz{Time: at.Add(-48 * time.Hour), Valid: true},
		QueryWindowEnd:   pgtype.Timestamptz{Time: at.Add(48 * time.Hour), Valid: true},
		RawPayload:       []byte(`{"transaction_id":"FIX-1001","status":"pending"}`),
	}
}

// each runs one case inside a savepoint of its own - pgx spells a nested
// Begin as one - and rolls it back afterwards, so a case that provokes a
// refusal leaves the outer transaction usable for the next.
func each(ctx context.Context, t *testing.T, tx pgx.Tx, name string, scenario func(t *testing.T, tx pgx.Tx, q *store.Queries)) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		sub, err := tx.Begin(ctx)
		if err != nil {
			t.Fatalf("savepoint: %v", err)
		}
		defer func() { _ = sub.Rollback(ctx) }()
		scenario(t, sub, store.New(sub))
	})
}

// refusal reports the SQLSTATE and the constraint a refusal names, or two
// empty strings when the error is not one the server raised.
//
// The constraint is checked as well as the code, and that is not
// belt-and-braces. Three different rules in 0012 refuse with 23505, and one
// of the cases below passed against a DELIBERATELY BROKEN digest because a
// second root tripped the one-root index at the same moment the digest
// constraint stopped tripping. A code alone cannot tell "the same report
// twice" from "two roots for one transaction", and those two say opposite
// things about whether the fingerprint works.
func refusal(err error) (code, constraint string) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code, pgErr.ConstraintName
	}
	return "", ""
}

func TestInsertNetworkTransactionAgainstSchema(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the networks store")
	}
	if err := db.Migrate(url); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer pool.Close()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	each(ctx, t, tx, "every column the network reported comes back unchanged", func(t *testing.T, tx pgx.Tx, queries *store.Queries) {
		networkID, accountID := account(ctx, t, tx)
		params := report(networkID, accountID)

		inserted, err := queries.InsertNetworkTransaction(ctx, params)
		if err != nil {
			t.Fatalf("InsertNetworkTransaction(): %v", err)
		}

		got, err := queries.GetNetworkTransaction(ctx, inserted.ID)
		if err != nil {
			t.Fatalf("GetNetworkTransaction(): %v", err)
		}
		if got.NetworkID != params.NetworkID || got.ExternalID != params.ExternalID {
			t.Errorf("stored %s/%s, want %s/%s", got.NetworkID, got.ExternalID, params.NetworkID, params.ExternalID)
		}
		if got.ClickRef != params.ClickRef {
			t.Errorf("stored click reference %v, want %v", got.ClickRef, params.ClickRef)
		}
		if got.StatusRaw != params.StatusRaw || got.Status != params.Status {
			t.Errorf("stored %s/%s, want %s/%s", got.StatusRaw, got.Status, params.StatusRaw, params.Status)
		}
		if got.SaleAmountMinor != params.SaleAmountMinor || got.CommissionMinor != params.CommissionMinor {
			t.Errorf("stored %d/%d minor units, want %d/%d",
				got.SaleAmountMinor, got.CommissionMinor, params.SaleAmountMinor, params.CommissionMinor)
		}
		if got.Currency != params.Currency {
			t.Errorf("stored currency %s, want %s", got.Currency, params.Currency)
		}
		if !got.TransactedAt.Time.Equal(params.TransactedAt.Time) {
			t.Errorf("stored transacted_at %s, want %s", got.TransactedAt.Time, params.TransactedAt.Time)
		}
		if !got.RetrievedAt.Time.Equal(params.RetrievedAt.Time) {
			t.Errorf("stored retrieved_at %s, want %s; the retrieval instant is the poller's to state, not the server's to default", got.RetrievedAt.Time, params.RetrievedAt.Time)
		}
		if !got.QueryWindowStart.Time.Equal(params.QueryWindowStart.Time) || !got.QueryWindowEnd.Time.Equal(params.QueryWindowEnd.Time) {
			t.Errorf("stored window %s..%s, want %s..%s",
				got.QueryWindowStart.Time, got.QueryWindowEnd.Time, params.QueryWindowStart.Time, params.QueryWindowEnd.Time)
		}
		if got.SupersedesID.Valid {
			t.Errorf("a first report was stored superseding %v; it is a root and must name no predecessor", got.SupersedesID)
		}

		// jsonb is not a byte store: it normalises whitespace and key order,
		// which is why the payload is compared as JSON rather than as bytes.
		// What FR-032 requires is that the FACTS survive verbatim, and a
		// re-encoding that changed one would show here.
		var stored, sent any
		if err := json.Unmarshal(got.RawPayload, &stored); err != nil {
			t.Fatalf("the stored payload is not JSON: %v", err)
		}
		if err := json.Unmarshal(params.RawPayload, &sent); err != nil {
			t.Fatalf("the sent payload is not JSON: %v", err)
		}
		storedJSON, _ := json.Marshal(stored)
		sentJSON, _ := json.Marshal(sent)
		if string(storedJSON) != string(sentJSON) {
			t.Errorf("the payload came back as %s, want %s", storedJSON, sentJSON)
		}
	})

	each(ctx, t, tx, "the digest is the database's and the caller never supplies it", func(t *testing.T, tx pgx.Tx, queries *store.Queries) {
		networkID, accountID := account(ctx, t, tx)

		inserted, err := queries.InsertNetworkTransaction(ctx, report(networkID, accountID))
		if err != nil {
			t.Fatalf("InsertNetworkTransaction(): %v", err)
		}
		// sha256, hex encoded. The length is the cheap half; the two cases
		// below are what say it is a fingerprint of the right thing.
		if len(inserted.ContentDigest) != 64 {
			t.Fatalf("the database returned a digest of %d characters (%q), want 64 hex characters of sha256",
				len(inserted.ContentDigest), inserted.ContentDigest)
		}
		if _, err := hex.DecodeString(inserted.ContentDigest); err != nil {
			t.Errorf("the digest %q is not hex: %v", inserted.ContentDigest, err)
		}

		stored, err := queries.GetNetworkTransaction(ctx, inserted.ID)
		if err != nil {
			t.Fatalf("GetNetworkTransaction(): %v", err)
		}
		if stored.ContentDigest != inserted.ContentDigest {
			t.Errorf("the insert returned %s and the row holds %s", inserted.ContentDigest, stored.ContentDigest)
		}
	})

	each(ctx, t, tx, "the payload is not part of the fingerprint", func(t *testing.T, tx pgx.Tx, queries *store.Queries) {
		// The property the migration argues for and nothing in Go can check:
		// networks stamp their own response timestamps and pagination
		// metadata into a payload, so a digest that hashed it would make
		// every re-report look like a change and every poll write a new row.
		networkID, accountID := account(ctx, t, tx)

		first := report(networkID, accountID)
		if _, err := queries.InsertNetworkTransaction(ctx, first); err != nil {
			t.Fatalf("the first report: %v", err)
		}

		second := report(networkID, accountID)
		second.RawPayload = []byte(`{"transaction_id":"FIX-1001","status":"pending","page":7,"served_at":"2026-08-03T11:00:00Z"}`)
		_, err := queries.InsertNetworkTransaction(ctx, second)
		code, constraint := refusal(err)
		if code != codeUniqueViolation {
			t.Fatalf("re-reporting the same facts under a different payload was accepted (SQLSTATE %q, err %v); an unchanged re-report must be the same row", code, err)
		}
		// Named, because a second root would also be 23505 and would mean
		// the digest had CHANGED - the opposite of what this case asserts.
		if constraint != "network_transaction_unique_report" {
			t.Errorf("the re-report was refused by %q, want network_transaction_unique_report; being stopped by any other rule means the digest saw the payload and changed", constraint)
		}
	})

	each(ctx, t, tx, "a changed fact is a different fingerprint", func(t *testing.T, tx pgx.Tx, queries *store.Queries) {
		networkID, accountID := account(ctx, t, tx)

		first := report(networkID, accountID)
		root, err := queries.InsertNetworkTransaction(ctx, first)
		if err != nil {
			t.Fatalf("the first report: %v", err)
		}

		// Every fact the trigger hashes, one at a time. Each must produce a
		// digest of its own, or a real change would collide with the report
		// it changed and be discarded as a duplicate.
		changes := map[string]func(*store.InsertNetworkTransactionParams){
			"the click reference": func(p *store.InsertNetworkTransactionParams) {
				p.ClickRef = pgtype.Text{String: "Zml4dHVyZS1jbGljay0wMDAwMDAwMg", Valid: true}
			},
			"the network's own word": func(p *store.InsertNetworkTransactionParams) { p.StatusRaw = "validated" },
			"the normalised status":  func(p *store.InsertNetworkTransactionParams) { p.Status = "confirmed" },
			"the sale amount":        func(p *store.InsertNetworkTransactionParams) { p.SaleAmountMinor = 5999 },
			"the commission":         func(p *store.InsertNetworkTransactionParams) { p.CommissionMinor = 599 },
			"the currency":           func(p *store.InsertNetworkTransactionParams) { p.Currency = "GBP" },
			"the transaction time": func(p *store.InsertNetworkTransactionParams) {
				p.TransactedAt = pgtype.Timestamptz{Time: p.TransactedAt.Time.Add(time.Second), Valid: true}
			},
		}
		// A savepoint per change, because the chain allows the root ONE
		// successor and this loop wants one per hashed fact.
		for name, change := range changes {
			each(ctx, t, tx, name, func(t *testing.T, _ pgx.Tx, queries *store.Queries) {
				changed := report(networkID, accountID)
				change(&changed)
				changed.SupersedesID = root.ID

				superseding, err := queries.InsertNetworkTransaction(ctx, changed)
				if err != nil {
					t.Fatalf("a report differing in %s was refused: %v", name, err)
				}
				if superseding.ContentDigest == root.ContentDigest {
					t.Errorf("changing %s produced the same digest %s; the change would be discarded as a duplicate", name, root.ContentDigest)
				}
			})
		}
	})

	each(ctx, t, tx, "an unattributed report is stored, and a blank reference is not", func(t *testing.T, tx pgx.Tx, queries *store.Queries) {
		networkID, accountID := account(ctx, t, tx)

		unattributed := report(networkID, accountID)
		unattributed.ClickRef = pgtype.Text{}
		stored, err := queries.InsertNetworkTransaction(ctx, unattributed)
		if err != nil {
			t.Fatalf("a transaction the network reported with no click reference was refused: %v; an unattributed report is evidence too (FR-034)", err)
		}
		if stored.ContentDigest == "" {
			t.Error("the unattributed report was stored without a digest")
		}

		blank := report(networkID, accountID)
		blank.ExternalID = "FIX-1002"
		blank.ClickRef = pgtype.Text{String: "   ", Valid: true}
		_, err = queries.InsertNetworkTransaction(ctx, blank)
		code, constraint := refusal(err)
		if code != codeCheckViolation {
			t.Fatalf("a blank click reference was accepted (SQLSTATE %q, err %v); blank and absent must not both be sayable, or an unattributed transaction sits in the attributed index carrying nothing", code, err)
		}
		if constraint != "network_transaction_click_ref_not_blank" {
			t.Errorf("the blank reference was refused by %q, want network_transaction_click_ref_not_blank", constraint)
		}
	})

	each(ctx, t, tx, "a transaction has one root", func(t *testing.T, tx pgx.Tx, queries *store.Queries) {
		networkID, accountID := account(ctx, t, tx)

		if _, err := queries.InsertNetworkTransaction(ctx, report(networkID, accountID)); err != nil {
			t.Fatalf("the first report: %v", err)
		}

		// Same transaction, different facts, but naming no predecessor. It
		// is a second root, and one root per transaction is what makes "the
		// current row" a well-defined thing.
		second := report(networkID, accountID)
		second.Status = "confirmed"
		second.StatusRaw = "validated"
		_, err := queries.InsertNetworkTransaction(ctx, second)
		code, constraint := refusal(err)
		if code != codeUniqueViolation {
			t.Fatalf("a second root for one transaction was accepted (SQLSTATE %q, err %v)", code, err)
		}
		if constraint != "network_transaction_one_root" {
			t.Errorf("the second root was refused by %q, want network_transaction_one_root", constraint)
		}
	})
}

// ifNew is the insert's conflict-swallowing twin, built from the same
// parameters so a case states one report rather than two shapes of it.
func ifNew(p store.InsertNetworkTransactionParams) store.InsertNetworkTransactionIfNewParams {
	return store.InsertNetworkTransactionIfNewParams(p)
}

// TestInsertNetworkTransactionIfNewAgainstSchema is where the ON CONFLICT
// clause earns its constraint name (T053).
//
// Everything here is about which of two rules a duplicate trips and what
// happens to the transaction afterwards. Neither can be read off the
// migration: Postgres decides which unique rule to report when a row
// violates several, and the fact that a swallowed conflict leaves the
// transaction usable is the whole reason this query exists rather than a Go
// error check.
func TestInsertNetworkTransactionIfNewAgainstSchema(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the networks store")
	}
	if err := db.Migrate(url); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer pool.Close()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	each(ctx, t, tx, "a first report is written and reported back", func(t *testing.T, tx pgx.Tx, queries *store.Queries) {
		networkID, accountID := account(ctx, t, tx)

		row, err := queries.InsertNetworkTransactionIfNew(ctx, ifNew(report(networkID, accountID)))
		if err != nil {
			t.Fatalf("InsertNetworkTransactionIfNew(): %v", err)
		}
		if !row.ID.Valid || len(row.ContentDigest) != 64 {
			t.Errorf("a written row came back as %+v, want an id and a 64-character digest", row)
		}
	})

	each(ctx, t, tx, "an unchanged re-report writes nothing and leaves the transaction usable", func(t *testing.T, tx pgx.Tx, queries *store.Queries) {
		networkID, accountID := account(ctx, t, tx)
		first := report(networkID, accountID)

		if _, err := queries.InsertNetworkTransactionIfNew(ctx, ifNew(first)); err != nil {
			t.Fatalf("the first report: %v", err)
		}

		// The same facts under a different payload, which is what a real
		// re-poll looks like: networks stamp their own response metadata in.
		again := first
		again.RawPayload = []byte(`{"transaction_id":"FIX-1001","status":"pending","page":7}`)
		again.RetrievedAt = pgtype.Timestamptz{Time: first.RetrievedAt.Time.Add(time.Hour), Valid: true}

		_, err := queries.InsertNetworkTransactionIfNew(ctx, ifNew(again))
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("the re-report returned %v, want pgx.ErrNoRows - no row written and no error raised", err)
		}

		// The half that a Go-side error check could never give: the
		// transaction is still alive, so the other reports of this window can
		// still be written. This is the whole reason the query exists.
		var stored int
		if err := tx.QueryRow(ctx, `select count(*) from cashback.network_transaction where network_id = $1`, networkID).Scan(&stored); err != nil {
			t.Fatalf("the transaction was aborted by a conflict it was supposed to swallow: %v", err)
		}
		if stored != 1 {
			t.Errorf("%d rows stored for one transaction, want 1", stored)
		}
	})

	each(ctx, t, tx, "a changed report written as a root still raises", func(t *testing.T, tx pgx.Tx, queries *store.Queries) {
		networkID, accountID := account(ctx, t, tx)

		if _, err := queries.InsertNetworkTransactionIfNew(ctx, ifNew(report(networkID, accountID))); err != nil {
			t.Fatalf("the first report: %v", err)
		}

		// A real status change, written without naming a predecessor. It is
		// NOT a duplicate, and swallowing it would leave the member's
		// confirmed transaction pending forever with nothing logged.
		changed := report(networkID, accountID)
		changed.StatusRaw = "validated"
		changed.Status = "confirmed"

		_, err := queries.InsertNetworkTransactionIfNew(ctx, ifNew(changed))
		code, constraint := refusal(err)
		if code != codeUniqueViolation {
			t.Fatalf("a changed report written as a root was accepted (SQLSTATE %q, err %v)", code, err)
		}
		if constraint != "network_transaction_one_root" {
			t.Errorf("it was refused by %q, want network_transaction_one_root; that name is what tells the ingestion path this should have superseded something rather than been discarded", constraint)
		}
	})

	each(ctx, t, tx, "a superseding report is written rather than swallowed", func(t *testing.T, tx pgx.Tx, queries *store.Queries) {
		networkID, accountID := account(ctx, t, tx)

		root, err := queries.InsertNetworkTransactionIfNew(ctx, ifNew(report(networkID, accountID)))
		if err != nil {
			t.Fatalf("the first report: %v", err)
		}

		changed := report(networkID, accountID)
		changed.StatusRaw = "validated"
		changed.Status = "confirmed"
		changed.SupersedesID = root.ID

		superseding, err := queries.InsertNetworkTransactionIfNew(ctx, ifNew(changed))
		if err != nil {
			t.Fatalf("a superseding report was refused: %v", err)
		}
		if superseding.ContentDigest == root.ContentDigest {
			t.Error("the superseding report carries the root's digest")
		}
	})
}

// TestGetCurrentNetworkTransactionAgainstSchema is the derivation the whole
// supersede path rests on: with an immutable table, "the current row" is not
// a column anybody set but a consequence of two constraints, and a query is
// where that consequence either holds or does not.
func TestGetCurrentNetworkTransactionAgainstSchema(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the networks store")
	}
	if err := db.Migrate(url); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer pool.Close()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	each(ctx, t, tx, "a transaction nobody reported has no current row", func(t *testing.T, tx pgx.Tx, queries *store.Queries) {
		networkID, _ := account(ctx, t, tx)

		_, err := queries.GetCurrentNetworkTransaction(ctx, store.GetCurrentNetworkTransactionParams{
			NetworkID:  networkID,
			ExternalID: "NEVER-REPORTED",
		})
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("GetCurrentNetworkTransaction() = %v, want pgx.ErrNoRows; a first report is not a failure", err)
		}
	})

	each(ctx, t, tx, "one report is its own current row", func(t *testing.T, tx pgx.Tx, queries *store.Queries) {
		networkID, accountID := account(ctx, t, tx)
		root, err := queries.InsertNetworkTransaction(ctx, report(networkID, accountID))
		if err != nil {
			t.Fatalf("the first report: %v", err)
		}

		current, err := queries.GetCurrentNetworkTransaction(ctx, store.GetCurrentNetworkTransactionParams{
			NetworkID:  networkID,
			ExternalID: "FIX-1001",
		})
		if err != nil {
			t.Fatalf("GetCurrentNetworkTransaction(): %v", err)
		}
		if current.ID != root.ID {
			t.Errorf("the current row is %v, want the only row %v", current.ID, root.ID)
		}
	})

	each(ctx, t, tx, "the tip moves to the superseding report and nowhere else", func(t *testing.T, tx pgx.Tx, queries *store.Queries) {
		networkID, accountID := account(ctx, t, tx)
		root, err := queries.InsertNetworkTransaction(ctx, report(networkID, accountID))
		if err != nil {
			t.Fatalf("the first report: %v", err)
		}

		// Three links, so the answer is the END of a chain rather than
		// merely "not the root": a query that returned the second row would
		// pass a two-link test and credit a member from a stale status.
		previous := root.ID
		var last pgtype.UUID
		for i, status := range []string{"confirmed", "reversed"} {
			changed := report(networkID, accountID)
			changed.Status = status
			changed.StatusRaw = status
			changed.SupersedesID = previous
			changed.RetrievedAt = pgtype.Timestamptz{Time: changed.RetrievedAt.Time.Add(time.Duration(i+1) * time.Hour), Valid: true}

			written, err := queries.InsertNetworkTransaction(ctx, changed)
			if err != nil {
				t.Fatalf("the %s report: %v", status, err)
			}
			previous, last = written.ID, written.ID
		}

		current, err := queries.GetCurrentNetworkTransaction(ctx, store.GetCurrentNetworkTransactionParams{
			NetworkID:  networkID,
			ExternalID: "FIX-1001",
		})
		if err != nil {
			t.Fatalf("GetCurrentNetworkTransaction(): %v", err)
		}
		if current.ID != last {
			t.Errorf("the current row is %v, want the tip %v; anything else credits a member from a status the network has already moved past", current.ID, last)
		}
		if current.Status != "reversed" {
			t.Errorf("the current row carries status %q, want reversed", current.Status)
		}
	})

	each(ctx, t, tx, "two transactions at one network keep their own tips", func(t *testing.T, tx pgx.Tx, queries *store.Queries) {
		networkID, accountID := account(ctx, t, tx)

		first, err := queries.InsertNetworkTransaction(ctx, report(networkID, accountID))
		if err != nil {
			t.Fatalf("the first transaction: %v", err)
		}
		other := report(networkID, accountID)
		other.ExternalID = "FIX-2002"
		second, err := queries.InsertNetworkTransaction(ctx, other)
		if err != nil {
			t.Fatalf("the second transaction: %v", err)
		}

		for external, want := range map[string]pgtype.UUID{"FIX-1001": first.ID, "FIX-2002": second.ID} {
			current, err := queries.GetCurrentNetworkTransaction(ctx, store.GetCurrentNetworkTransactionParams{
				NetworkID:  networkID,
				ExternalID: external,
			})
			if err != nil {
				t.Fatalf("GetCurrentNetworkTransaction(%s): %v", external, err)
			}
			if current.ID != want {
				t.Errorf("the current row for %s is %v, want %v", external, current.ID, want)
			}
		}
	})

	each(ctx, t, tx, "one network's chain is not another's", func(t *testing.T, tx pgx.Tx, queries *store.Queries) {
		// The same external id at two networks is ordinary - a network's ids
		// are its own - so the read is keyed by both or it answers with
		// somebody else's transaction.
		//
		// BOTH sides are read, and that is what makes this decisive. Asking
		// only for ours would pass whenever the row store happened to hand
		// ours back first, which is exactly how this case passed against a
		// query with network_id dropped from the key. Two reads with two
		// different right answers cannot both be satisfied by a query that
		// can only give one.
		mine, myAccount := account(ctx, t, tx)
		theirs, theirAccount := account(ctx, t, tx)

		ours, err := queries.InsertNetworkTransaction(ctx, report(mine, myAccount))
		if err != nil {
			t.Fatalf("our report: %v", err)
		}
		notOurs, err := queries.InsertNetworkTransaction(ctx, report(theirs, theirAccount))
		if err != nil {
			t.Fatalf("their report: %v", err)
		}

		for networkID, want := range map[string]pgtype.UUID{mine: ours.ID, theirs: notOurs.ID} {
			current, err := queries.GetCurrentNetworkTransaction(ctx, store.GetCurrentNetworkTransactionParams{
				NetworkID:  networkID,
				ExternalID: "FIX-1001",
			})
			if err != nil {
				t.Fatalf("GetCurrentNetworkTransaction(%s): %v", networkID, err)
			}
			if current.ID != want {
				t.Errorf("the current row at %s is %v, want %v; a read keyed on the transaction id alone answers with another network's transaction", networkID, current.ID, want)
			}
			if current.NetworkID != networkID {
				t.Errorf("the row returned for %s belongs to %s", networkID, current.NetworkID)
			}
		}
	})
}
