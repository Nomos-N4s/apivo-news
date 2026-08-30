// The tests for digest.go: which re-report is a no-op, which is a failure,
// and the difference between the two.

package networks_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// dedupTestStore answers the three ways the query can: a row, no rows, or a
// refusal. It records what it was asked so a case can assert that nothing
// reached the database when a value was refused.
type dedupTestStore struct {
	got  []store.InsertNetworkTransactionIfNewParams
	row  store.InsertNetworkTransactionIfNewRow
	fail error
}

func (s *dedupTestStore) InsertNetworkTransactionIfNew(_ context.Context, arg store.InsertNetworkTransactionIfNewParams) (store.InsertNetworkTransactionIfNewRow, error) {
	s.got = append(s.got, arg)
	if s.fail != nil {
		return store.InsertNetworkTransactionIfNewRow{}, s.fail
	}
	return s.row, nil
}

func TestDeduplicatorNeedsAStore(t *testing.T) {
	t.Parallel()

	if _, err := networks.NewDeduplicator(nil); !errors.Is(err, networks.ErrNoEvidenceStore) {
		t.Fatalf("NewDeduplicator(nil) = %v, want one wrapping ErrNoEvidenceStore", err)
	}
	if _, err := networks.NewDeduplicator(&dedupTestStore{}); err != nil {
		t.Fatalf("NewDeduplicator() refused a usable store: %v", err)
	}
}

// TestRecordIfNewAnswersThreeWays pins the shape of the answer, and the
// middle one is the whole task: no row written, no error, and the rest of
// the window still writable.
func TestRecordIfNewAnswersThreeWays(t *testing.T) {
	t.Parallel()

	t.Run("a new report is written and reported", func(t *testing.T) {
		t.Parallel()
		id := uuid.New()
		fake := &dedupTestStore{row: store.InsertNetworkTransactionIfNewRow{
			ID:            pgtype.UUID{Bytes: id, Valid: true},
			ContentDigest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			RetrievedAt:   pgtype.Timestamptz{Time: time.Date(2026, time.August, 10, 6, 0, 0, 0, time.UTC), Valid: true},
		}}
		dedup, err := networks.NewDeduplicator(fake)
		if err != nil {
			t.Fatalf("NewDeduplicator(): %v", err)
		}

		recorded, written, err := dedup.RecordIfNew(t.Context(), evidenceTestRetrieval(t), evidenceTestReport(t))
		if err != nil {
			t.Fatalf("RecordIfNew(): %v", err)
		}
		if !written {
			t.Error("a report the database wrote was reported as already stored")
		}
		if recorded.ID != id {
			t.Errorf("ID = %s, want %s", recorded.ID, id)
		}
	})

	t.Run("an unchanged re-report is a silent no-op", func(t *testing.T) {
		t.Parallel()
		// pgx.ErrNoRows is what the query's `do nothing` produces: the
		// statement ran, nothing was written, and the transaction is intact.
		fake := &dedupTestStore{fail: pgx.ErrNoRows}
		dedup, err := networks.NewDeduplicator(fake)
		if err != nil {
			t.Fatalf("NewDeduplicator(): %v", err)
		}

		recorded, written, err := dedup.RecordIfNew(t.Context(), evidenceTestRetrieval(t), evidenceTestReport(t))
		if err != nil {
			t.Fatalf("an unchanged re-report was reported as a failure: %v; a poller that logged every one of these would log most of every trailing re-read", err)
		}
		if written {
			t.Error("a report nothing was written for was reported as written")
		}
		if recorded != (networks.Recorded{}) {
			t.Errorf("a skipped report came back as %+v, want the zero value: the database reports nothing about the row it matched", recorded)
		}
	})

	t.Run("anything else is a failure carrying the cause", func(t *testing.T) {
		t.Parallel()
		refused := errors.New(`ERROR: duplicate key value violates unique constraint "network_transaction_one_root"`)
		dedup, err := networks.NewDeduplicator(&dedupTestStore{fail: refused})
		if err != nil {
			t.Fatalf("NewDeduplicator(): %v", err)
		}

		_, written, err := dedup.RecordIfNew(t.Context(), evidenceTestRetrieval(t), evidenceTestReport(t))
		if !errors.Is(err, networks.ErrEvidenceNotWritten) {
			t.Fatalf("RecordIfNew() = %v, want one wrapping ErrEvidenceNotWritten", err)
		}
		if !errors.Is(err, refused) {
			t.Errorf("RecordIfNew() = %v, which does not carry the database's own refusal; the supersede path reads it", err)
		}
		if written {
			t.Error("a refused report was reported as written")
		}
	})
}

// TestRecordIfNewRefusesBeforeItWrites is the same rule the evidence writer
// keeps, held separately because this is a separate path into the same
// immutable table.
func TestRecordIfNewRefusesBeforeItWrites(t *testing.T) {
	t.Parallel()

	noPayload := evidenceTestReport(t)
	noPayload.RawPayload = nil

	fake := &dedupTestStore{}
	dedup, err := networks.NewDeduplicator(fake)
	if err != nil {
		t.Fatalf("NewDeduplicator(): %v", err)
	}

	if _, _, err := dedup.RecordIfNew(t.Context(), evidenceTestRetrieval(t), noPayload); !errors.Is(err, networks.ErrMissingRawPayload) {
		t.Fatalf("RecordIfNew() = %v, want one wrapping ErrMissingRawPayload", err)
	}
	noMoment := networks.Retrieval{Account: retrievalTestAccount(t), Window: retrievalTestWindow()}
	if _, _, err := dedup.RecordIfNew(t.Context(), noMoment, evidenceTestReport(t)); !errors.Is(err, networks.ErrInvalidRetrieval) {
		t.Fatalf("RecordIfNew() = %v, want one wrapping ErrInvalidRetrieval", err)
	}
	if len(fake.got) != 0 {
		t.Errorf("the dedup path sent %d insert(s) for values it refused; the row would be permanent", len(fake.got))
	}
}

// TestRecordIfNewAgainstTheRealSchema is the layer no fake can stand in for.
// Which conflict the server picks, and whether a swallowed one leaves the
// transaction usable, are the two things this whole path is built on.
func TestRecordIfNewAgainstTheRealSchema(t *testing.T) {
	t.Parallel()

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the dedup path")
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

	networkID := "fixture_dedup"
	if _, err := tx.Exec(ctx, `
		insert into cashback.network (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_second, active)
		values ($1, 'Dedup Network', 'clickref', 31, 6, true)`, networkID); err != nil {
		t.Fatalf("seeding the network: %v", err)
	}
	var accountID pgtype.UUID
	if err := tx.QueryRow(ctx, `
		insert into cashback.network_account (network_id, external_publisher_id, credential_ref, active)
		values ($1, 'publisher-1', 'config:networks.fixture.credential', true)
		returning id`, networkID).Scan(&accountID); err != nil {
		t.Fatalf("seeding the publisher account: %v", err)
	}
	account, err := networks.NewPublisherAccount(uuid.UUID(accountID.Bytes), networks.NetworkID(networkID), "publisher-1")
	if err != nil {
		t.Fatalf("NewPublisherAccount(): %v", err)
	}
	retrieval := networks.Retrieval{
		Account:     account,
		RetrievedAt: time.Date(2026, time.August, 10, 6, 0, 0, 0, time.UTC),
		Window:      retrievalTestWindow(),
	}

	dedup, err := networks.NewDeduplicator(store.New(tx))
	if err != nil {
		t.Fatalf("NewDeduplicator(): %v", err)
	}

	report := evidenceTestReport(t)
	first, written, err := dedup.RecordIfNew(ctx, retrieval, report)
	if err != nil {
		t.Fatalf("the first report: %v", err)
	}
	if !written {
		t.Fatal("the first report was reported as already stored")
	}
	if first.ID == uuid.Nil || len(first.ContentDigest) != 64 {
		t.Fatalf("the first report came back as %+v, want an id and the database's 64-character digest", first)
	}

	// The re-poll a trailing window really produces: the same facts inside
	// different bytes, read at a different moment.
	again := report
	again.RawPayload = []byte(`{"transaction_id":"FIX-1001","status":"pending","page":3,"served_at":"2026-08-11T00:00:00Z"}`)
	laterRetrieval := retrieval
	laterRetrieval.RetrievedAt = retrieval.RetrievedAt.Add(24 * time.Hour)

	skipped, written, err := dedup.RecordIfNew(ctx, laterRetrieval, again)
	if err != nil {
		t.Fatalf("an unchanged re-report failed: %v", err)
	}
	if written {
		t.Error("an unchanged re-report wrote a second row")
	}
	if skipped != (networks.Recorded{}) {
		t.Errorf("a skipped report came back as %+v, want the zero value", skipped)
	}

	// The half a fake cannot prove: the transaction survived the swallowed
	// conflict, so the rest of the window is still writable. Without this,
	// a poller would lose every report after the first duplicate.
	other := evidenceTestReport(t)
	other.ExternalID = "FIX-2002"
	if _, written, err := dedup.RecordIfNew(ctx, retrieval, other); err != nil || !written {
		t.Fatalf("the report after a swallowed conflict failed (written=%v): %v; the conflict aborted the window's transaction", written, err)
	}

	// And a CHANGED report written as a root is not a duplicate: it is the
	// supersede path's business, and discarding it would leave a member's
	// confirmed transaction pending forever.
	changed := report
	changed.StatusRaw = "validated"
	changed.Status = networks.StatusConfirmed
	if _, written, err := dedup.RecordIfNew(ctx, retrieval, changed); err == nil || written {
		t.Fatalf("a changed report written as a root was accepted (written=%v, err=%v)", written, err)
	} else if !errors.Is(err, networks.ErrEvidenceNotWritten) {
		t.Errorf("it failed with %v, want one wrapping ErrEvidenceNotWritten", err)
	}
}
