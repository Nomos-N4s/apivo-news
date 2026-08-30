// The tests for supersede.go: which report becomes a new row, which becomes
// nothing, and what happens when two pollers reach for the same predecessor.

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

// supersedeTestChain answers the chain read: a row, no rows, or a failure.
type supersedeTestChain struct {
	asked []store.GetCurrentNetworkTransactionParams
	row   store.CashbackNetworkTransaction
	fail  error
}

func (c *supersedeTestChain) GetCurrentNetworkTransaction(_ context.Context, arg store.GetCurrentNetworkTransactionParams) (store.CashbackNetworkTransaction, error) {
	c.asked = append(c.asked, arg)
	if c.fail != nil {
		return store.CashbackNetworkTransaction{}, c.fail
	}
	return c.row, nil
}

func TestSupersederNeedsBothStores(t *testing.T) {
	t.Parallel()

	if _, err := networks.NewSuperseder(nil, &dedupTestStore{}); !errors.Is(err, networks.ErrNoEvidenceStore) {
		t.Errorf("NewSuperseder(nil, store) = %v, want one wrapping ErrNoEvidenceStore", err)
	}
	if _, err := networks.NewSuperseder(&supersedeTestChain{}, nil); !errors.Is(err, networks.ErrNoEvidenceStore) {
		t.Errorf("NewSuperseder(chain, nil) = %v, want one wrapping ErrNoEvidenceStore", err)
	}
	if _, err := networks.NewSuperseder(&supersedeTestChain{}, &dedupTestStore{}); err != nil {
		t.Errorf("NewSuperseder() refused two usable stores: %v", err)
	}
}

// TestRecordNamesThePredecessorItRead is the one decision Go makes here. The
// digest decides whether a report is a change; this decides which row a
// change is hung from, and hanging it from the wrong one forks a
// transaction's history.
func TestRecordNamesThePredecessorItRead(t *testing.T) {
	t.Parallel()

	tip := uuid.New()
	chain := &supersedeTestChain{row: store.CashbackNetworkTransaction{ID: pgtype.UUID{Bytes: tip, Valid: true}}}
	write := &dedupTestStore{row: store.InsertNetworkTransactionIfNewRow{
		ID:            pgtype.UUID{Bytes: uuid.New(), Valid: true},
		ContentDigest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		RetrievedAt:   pgtype.Timestamptz{Time: time.Date(2026, time.August, 10, 6, 0, 0, 0, time.UTC), Valid: true},
	}}
	superseder, err := networks.NewSuperseder(chain, write)
	if err != nil {
		t.Fatalf("NewSuperseder(): %v", err)
	}

	retrieval, report := evidenceTestRetrieval(t), evidenceTestReport(t)
	_, outcome, err := superseder.Record(t.Context(), retrieval, report)
	if err != nil {
		t.Fatalf("Record(): %v", err)
	}
	if outcome != networks.OutcomeSuperseded {
		t.Errorf("outcome = %q, want %q", outcome, networks.OutcomeSuperseded)
	}

	if len(chain.asked) != 1 {
		t.Fatalf("the chain was read %d time(s), want 1", len(chain.asked))
	}
	if got := chain.asked[0]; got.NetworkID != string(retrieval.Account.Network()) || got.ExternalID != report.ExternalID {
		t.Errorf("the chain was read for %s/%s, want %s/%s; a read keyed on the transaction id alone finds another network's chain",
			got.NetworkID, got.ExternalID, retrieval.Account.Network(), report.ExternalID)
	}

	if len(write.got) != 1 {
		t.Fatalf("the writer made %d insert(s), want 1", len(write.got))
	}
	if uuid.UUID(write.got[0].SupersedesID.Bytes) != tip || !write.got[0].SupersedesID.Valid {
		t.Errorf("the new row names %v as its predecessor, want the tip %s", write.got[0].SupersedesID, tip)
	}
}

// TestRecordWritesARootWhenThereIsNothingToSupersede holds the other half:
// no rows from the chain read is a FIRST REPORT, not a failure, and the row
// written must name no predecessor or the guard trigger refuses it.
func TestRecordWritesARootWhenThereIsNothingToSupersede(t *testing.T) {
	t.Parallel()

	chain := &supersedeTestChain{fail: pgx.ErrNoRows}
	write := &dedupTestStore{row: store.InsertNetworkTransactionIfNewRow{
		ID:            pgtype.UUID{Bytes: uuid.New(), Valid: true},
		ContentDigest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		RetrievedAt:   pgtype.Timestamptz{Time: time.Date(2026, time.August, 10, 6, 0, 0, 0, time.UTC), Valid: true},
	}}
	superseder, err := networks.NewSuperseder(chain, write)
	if err != nil {
		t.Fatalf("NewSuperseder(): %v", err)
	}

	_, outcome, err := superseder.Record(t.Context(), evidenceTestRetrieval(t), evidenceTestReport(t))
	if err != nil {
		t.Fatalf("Record(): %v", err)
	}
	if outcome != networks.OutcomeFirstReport {
		t.Errorf("outcome = %q, want %q", outcome, networks.OutcomeFirstReport)
	}
	if len(write.got) != 1 || write.got[0].SupersedesID.Valid {
		t.Errorf("a first report named %v as its predecessor; a root names none", write.got[0].SupersedesID)
	}
}

// TestRecordReportsAnUnchangedReportAsUnchanged is the outcome most of every
// trailing re-read produces, and the one that must be silent.
func TestRecordReportsAnUnchangedReportAsUnchanged(t *testing.T) {
	t.Parallel()

	chain := &supersedeTestChain{row: store.CashbackNetworkTransaction{ID: pgtype.UUID{Bytes: uuid.New(), Valid: true}}}
	superseder, err := networks.NewSuperseder(chain, &dedupTestStore{fail: pgx.ErrNoRows})
	if err != nil {
		t.Fatalf("NewSuperseder(): %v", err)
	}

	recorded, outcome, err := superseder.Record(t.Context(), evidenceTestRetrieval(t), evidenceTestReport(t))
	if err != nil {
		t.Fatalf("an unchanged re-report was reported as a failure: %v", err)
	}
	if outcome != networks.OutcomeUnchanged {
		t.Errorf("outcome = %q, want %q", outcome, networks.OutcomeUnchanged)
	}
	if recorded != (networks.Recorded{}) {
		t.Errorf("an unchanged report came back as %+v, want the zero value", recorded)
	}
}

// TestRecordRefusesBeforeItReadsOrWrites keeps the order. Both stores must
// be untouched: the row is immutable, and a chain read for a value that was
// never going to be written is a question nobody needed answered.
func TestRecordRefusesBeforeItReadsOrWrites(t *testing.T) {
	t.Parallel()

	noPayload := evidenceTestReport(t)
	noPayload.RawPayload = nil

	chain, write := &supersedeTestChain{}, &dedupTestStore{}
	superseder, err := networks.NewSuperseder(chain, write)
	if err != nil {
		t.Fatalf("NewSuperseder(): %v", err)
	}

	if _, _, err := superseder.Record(t.Context(), evidenceTestRetrieval(t), noPayload); !errors.Is(err, networks.ErrMissingRawPayload) {
		t.Fatalf("Record() = %v, want one wrapping ErrMissingRawPayload", err)
	}
	noMoment := networks.Retrieval{Account: retrievalTestAccount(t), Window: retrievalTestWindow()}
	if _, _, err := superseder.Record(t.Context(), noMoment, evidenceTestReport(t)); !errors.Is(err, networks.ErrInvalidRetrieval) {
		t.Fatalf("Record() = %v, want one wrapping ErrInvalidRetrieval", err)
	}
	if len(chain.asked) != 0 || len(write.got) != 0 {
		t.Errorf("a refused report reached the stores: %d read(s), %d write(s)", len(chain.asked), len(write.got))
	}
}

// TestSupersedeAgainstTheRealSchema plays the recorded lifecycle through the
// database that arbitrates it. Whether a report is a change is decided by a
// trigger and a constraint, and nothing short of the real schema can say so.
func TestSupersedeAgainstTheRealSchema(t *testing.T) {
	t.Parallel()

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the supersede path")
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

	networkID := "fixture_supersede"
	if _, err := tx.Exec(ctx, `
		insert into cashback.network (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_second, active)
		values ($1, 'Supersede Network', 'clickref', 31, 6, true)`, networkID); err != nil {
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

	queries := store.New(tx)
	superseder, err := networks.NewSuperseder(queries, queries)
	if err != nil {
		t.Fatalf("NewSuperseder(): %v", err)
	}

	// The lifecycle the whole ingestion chain exists to handle, one poll at
	// a time: first word, the same word again, then a change, then the same
	// change again.
	report := evidenceTestReport(t)

	first, outcome, err := superseder.Record(ctx, retrieval, report)
	if err != nil {
		t.Fatalf("the first report: %v", err)
	}
	if outcome != networks.OutcomeFirstReport {
		t.Fatalf("the first report was %q, want %q", outcome, networks.OutcomeFirstReport)
	}

	if _, outcome, err = superseder.Record(ctx, retrieval, report); err != nil || outcome != networks.OutcomeUnchanged {
		t.Fatalf("re-reporting the same facts was %q (err %v), want %q", outcome, err, networks.OutcomeUnchanged)
	}

	confirmed := report
	confirmed.StatusRaw = "validated"
	confirmed.Status = networks.StatusConfirmed
	second, outcome, err := superseder.Record(ctx, retrieval, confirmed)
	if err != nil {
		t.Fatalf("the confirmed report: %v", err)
	}
	if outcome != networks.OutcomeSuperseded {
		t.Fatalf("a changed status was %q, want %q", outcome, networks.OutcomeSuperseded)
	}
	if second.ID == first.ID || second.ContentDigest == first.ContentDigest {
		t.Error("the superseding report reused the first report's row or digest")
	}

	if _, outcome, err = superseder.Record(ctx, retrieval, confirmed); err != nil || outcome != networks.OutcomeUnchanged {
		t.Fatalf("re-reporting the confirmed facts was %q (err %v), want %q", outcome, err, networks.OutcomeUnchanged)
	}

	// C-3: the first row is untouched. What the network said in the first
	// poll stays exactly as readable as what it says now, which is what an
	// operator needs on the day a member asks why a confirmed purchase was
	// reversed.
	original, err := queries.GetNetworkTransaction(ctx, pgtype.UUID{Bytes: first.ID, Valid: true})
	if err != nil {
		t.Fatalf("reading the superseded row: %v", err)
	}
	if original.Status != string(report.Status) || original.ContentDigest != first.ContentDigest {
		t.Errorf("the superseded row now reads %s/%s, want %s/%s; superseding must not edit",
			original.Status, original.ContentDigest, report.Status, first.ContentDigest)
	}

	// And the chain's tip has moved to the newest row and nowhere else.
	current, err := queries.GetCurrentNetworkTransaction(ctx, store.GetCurrentNetworkTransactionParams{
		NetworkID:  networkID,
		ExternalID: report.ExternalID,
	})
	if err != nil {
		t.Fatalf("GetCurrentNetworkTransaction(): %v", err)
	}
	if uuid.UUID(current.ID.Bytes) != second.ID {
		t.Errorf("the current row is %v, want the confirmed report %s", current.ID, second.ID)
	}
}

// TestRecordReportsALostSupersedeRace is the failure two pollers can
// produce, and the reason it has a sentinel of its own: the answer is to
// re-read the tip and try again, not to investigate. A caller that retried
// every write failure would keep retrying an expired credential forever.
func TestRecordReportsALostSupersedeRace(t *testing.T) {
	t.Parallel()

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the supersede path")
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

	networkID := "fixture_race"
	if _, err := tx.Exec(ctx, `
		insert into cashback.network (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_second, active)
		values ($1, 'Race Network', 'clickref', 31, 6, true)`, networkID); err != nil {
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
	queries := store.New(tx)

	report := evidenceTestReport(t)
	live, err := networks.NewSuperseder(queries, queries)
	if err != nil {
		t.Fatalf("NewSuperseder(): %v", err)
	}
	root, _, err := live.Record(ctx, retrieval, report)
	if err != nil {
		t.Fatalf("the first report: %v", err)
	}

	// One poller supersedes. Then a second, still holding the tip it read a
	// moment earlier, offers its own successor for the same row. The
	// sequence is written out rather than run concurrently: what is being
	// proved is the chain's rule and the error it produces, and a real race
	// would prove the same thing less often.
	confirmed := report
	confirmed.StatusRaw = "validated"
	confirmed.Status = networks.StatusConfirmed
	if _, outcome, err := live.Record(ctx, retrieval, confirmed); err != nil || outcome != networks.OutcomeSuperseded {
		t.Fatalf("the winning supersede was %q (err %v)", outcome, err)
	}

	stale := &supersedeTestChain{row: store.CashbackNetworkTransaction{ID: pgtype.UUID{Bytes: root.ID, Valid: true}}}
	loser, err := networks.NewSuperseder(stale, queries)
	if err != nil {
		t.Fatalf("NewSuperseder(): %v", err)
	}
	reversed := report
	reversed.StatusRaw = "chargeback"
	reversed.Status = networks.StatusReversed

	_, outcome, err := loser.Record(ctx, retrieval, reversed)
	if !errors.Is(err, networks.ErrSupersededConcurrently) {
		t.Fatalf("the losing supersede returned %q / %v, want one wrapping ErrSupersededConcurrently", outcome, err)
	}
	if errors.Is(err, networks.ErrEvidenceNotWritten) {
		t.Error("a lost race reads as an ordinary write failure; the answer to it is to re-read the tip, not to investigate")
	}
}
