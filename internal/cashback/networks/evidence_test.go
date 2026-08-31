// The tests for evidence.go: what the writer builds, what it refuses before
// writing anything, and what it reports back from the database.
//
// Two layers, for two different questions. A fake store answers "what
// parameters did it build", which is the writer's whole job and cannot be
// read off a stored row - a column that arrived by the server's default and
// one the writer stated look identical afterwards. A real Postgres answers
// "does the row it builds actually store", which no fake can be trusted to
// say, because the fake would be agreeing with whatever this file believed
// the schema wanted.

package networks_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// evidenceTestStore records what it was asked to insert and answers with what
// a database would have decided. It never validates: a fake that refused the
// same things the schema refuses would be this file's opinion of the schema,
// and the Postgres case below is what holds that opinion to account.
type evidenceTestStore struct {
	got  []store.InsertNetworkTransactionParams
	row  store.InsertNetworkTransactionRow
	fail error
}

func (s *evidenceTestStore) InsertNetworkTransaction(_ context.Context, arg store.InsertNetworkTransactionParams) (store.InsertNetworkTransactionRow, error) {
	s.got = append(s.got, arg)
	if s.fail != nil {
		return store.InsertNetworkTransactionRow{}, s.fail
	}
	return s.row, nil
}

func evidenceTestRetrieval(t *testing.T) networks.Retrieval {
	t.Helper()
	return networks.Retrieval{
		Account:     retrievalTestAccount(t),
		RetrievedAt: time.Date(2026, time.August, 10, 6, 0, 0, 0, time.UTC),
		Window:      retrievalTestWindow(),
	}
}

func evidenceTestReport(t *testing.T) networks.Reported {
	t.Helper()
	sale, err := money.New(4999, money.Currency("EUR"))
	if err != nil {
		t.Fatalf("sale amount: %v", err)
	}
	commission, err := money.New(499, money.Currency("EUR"))
	if err != nil {
		t.Fatalf("commission: %v", err)
	}
	return networks.Reported{
		ExternalID:   "FIX-1001",
		ClickRef:     networks.NewClickRef("Zml4dHVyZS1jbGljay0wMDAwMDAwMQ"),
		StatusRaw:    "pending",
		Status:       networks.StatusPending,
		SaleAmount:   sale,
		Commission:   commission,
		TransactedAt: time.Date(2026, time.August, 3, 9, 15, 0, 0, time.UTC),
		RawPayload:   json.RawMessage(`{"transaction_id":"FIX-1001","status":"pending"}`),
	}
}

// TestEvidenceWriterNeedsAStore refuses at construction rather than at the
// first report. A poller that discovered it mid-window would already have
// read a window it cannot persist, and would have to decide what to do with
// the half of it still in memory.
func TestEvidenceWriterNeedsAStore(t *testing.T) {
	t.Parallel()

	if _, err := networks.NewEvidenceWriter(nil); !errors.Is(err, networks.ErrNoEvidenceStore) {
		t.Fatalf("NewEvidenceWriter(nil) = %v, want one wrapping ErrNoEvidenceStore", err)
	}
	if _, err := networks.NewEvidenceWriter(&evidenceTestStore{}); err != nil {
		t.Fatalf("NewEvidenceWriter() refused a usable store: %v", err)
	}
}

// TestRecordBuildsTheRowFromTheReportAndTheRetrieval is the writer's whole
// job, and the layer a stored row cannot answer for: a column the writer
// stated and one that arrived by the server's default look identical
// afterwards.
func TestRecordBuildsTheRowFromTheReportAndTheRetrieval(t *testing.T) {
	t.Parallel()

	fake := &evidenceTestStore{row: store.InsertNetworkTransactionRow{
		ID:            pgtype.UUID{Bytes: uuid.New(), Valid: true},
		ContentDigest: strings.Repeat("a", 64),
		RetrievedAt:   pgtype.Timestamptz{Time: time.Date(2026, time.August, 10, 6, 0, 0, 0, time.UTC), Valid: true},
	}}
	writer, err := networks.NewEvidenceWriter(fake)
	if err != nil {
		t.Fatalf("NewEvidenceWriter(): %v", err)
	}

	retrieval, report := evidenceTestRetrieval(t), evidenceTestReport(t)
	if _, err := writer.Record(t.Context(), retrieval, report); err != nil {
		t.Fatalf("Record(): %v", err)
	}
	if len(fake.got) != 1 {
		t.Fatalf("the writer made %d insert(s), want 1", len(fake.got))
	}
	got := fake.got[0]

	if got.NetworkID != string(retrieval.Account.Network()) {
		t.Errorf("network_id = %q, want %q", got.NetworkID, retrieval.Account.Network())
	}
	if uuid.UUID(got.NetworkAccountID.Bytes) != retrieval.Account.ID() || !got.NetworkAccountID.Valid {
		t.Errorf("network_account_id = %v, want %s", got.NetworkAccountID, retrieval.Account.ID())
	}
	if got.ExternalID != report.ExternalID {
		t.Errorf("external_id = %q, want %q", got.ExternalID, report.ExternalID)
	}
	ref, _ := report.ClickRef.Ref()
	if got.ClickRef.String != ref || !got.ClickRef.Valid {
		t.Errorf("click_ref = %v, want %q", got.ClickRef, ref)
	}
	if got.StatusRaw != report.StatusRaw || got.Status != string(report.Status) {
		t.Errorf("status_raw/status = %q/%q, want %q/%q", got.StatusRaw, got.Status, report.StatusRaw, report.Status)
	}
	if got.SaleAmountMinor != report.SaleAmount.Minor || got.CommissionMinor != report.Commission.Minor {
		t.Errorf("amounts = %d/%d, want %d/%d", got.SaleAmountMinor, got.CommissionMinor, report.SaleAmount.Minor, report.Commission.Minor)
	}
	if got.Currency != string(report.SaleAmount.Currency) {
		t.Errorf("currency = %q, want %q", got.Currency, report.SaleAmount.Currency)
	}
	if !got.TransactedAt.Time.Equal(report.TransactedAt) {
		t.Errorf("transacted_at = %s, want %s", got.TransactedAt.Time, report.TransactedAt)
	}
	// The three the port does not carry, and the reason Retrieval exists.
	if !got.RetrievedAt.Time.Equal(retrieval.RetrievedAt) {
		t.Errorf("retrieved_at = %s, want %s; the instant is the poller's to state, or a window that took a minute to persist reads as a minute of separate retrievals", got.RetrievedAt.Time, retrieval.RetrievedAt)
	}
	if !got.QueryWindowStart.Time.Equal(retrieval.Window.From) || !got.QueryWindowEnd.Time.Equal(retrieval.Window.To) {
		t.Errorf("window = %s..%s, want %s..%s", got.QueryWindowStart.Time, got.QueryWindowEnd.Time, retrieval.Window.From, retrieval.Window.To)
	}
	if string(got.RawPayload) != string(report.RawPayload) {
		t.Errorf("raw_payload = %s, want %s", got.RawPayload, report.RawPayload)
	}
	// A root names no predecessor. A superseding report is T054's, and a
	// writer that set this without being told to would attach a chain to
	// whatever happened to be there.
	if got.SupersedesID.Valid {
		t.Errorf("supersedes_id = %v; Record writes a root and must name no predecessor", got.SupersedesID)
	}
}

// TestRecordReportsWhatTheDatabaseDecided rather than what it was handed. The
// digest is computed by a trigger and a caller-supplied one is discarded, so
// a writer that echoed its inputs back would report success for a row the
// database had rewritten underneath it.
func TestRecordReportsWhatTheDatabaseDecided(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	decided := time.Date(2031, time.January, 2, 3, 4, 5, 0, time.UTC)
	fake := &evidenceTestStore{row: store.InsertNetworkTransactionRow{
		ID:            pgtype.UUID{Bytes: id, Valid: true},
		ContentDigest: strings.Repeat("b", 64),
		// Deliberately not the instant the retrieval states: what comes back
		// is what the row holds.
		RetrievedAt: pgtype.Timestamptz{Time: decided, Valid: true},
	}}
	writer, err := networks.NewEvidenceWriter(fake)
	if err != nil {
		t.Fatalf("NewEvidenceWriter(): %v", err)
	}

	recorded, err := writer.Record(t.Context(), evidenceTestRetrieval(t), evidenceTestReport(t))
	if err != nil {
		t.Fatalf("Record(): %v", err)
	}
	if recorded.ID != id {
		t.Errorf("ID = %s, want %s", recorded.ID, id)
	}
	if recorded.ContentDigest != strings.Repeat("b", 64) {
		t.Errorf("ContentDigest = %q, want the one the database returned", recorded.ContentDigest)
	}
	if !recorded.RetrievedAt.Equal(decided) {
		t.Errorf("RetrievedAt = %s, want %s - the row's value, not the caller's", recorded.RetrievedAt, decided)
	}
}

// TestRecordRefusesBeforeItWrites holds the order, and the order is the
// point: this row is IMMUTABLE, so a bad one cannot be corrected, only
// superseded, and it would sit in the evidence a member's money rests on
// forever. Nothing reaches the store until both halves are whole.
func TestRecordRefusesBeforeItWrites(t *testing.T) {
	t.Parallel()

	valid := evidenceTestReport(t)
	noPayload := valid
	noPayload.RawPayload = nil
	unmapped := valid
	unmapped.Status = networks.Status("validated")

	cases := map[string]struct {
		retrieval networks.Retrieval
		report    networks.Reported
		want      error
	}{
		"a retrieval naming no moment": {
			retrieval: networks.Retrieval{Account: retrievalTestAccount(t), Window: retrievalTestWindow()},
			report:    valid,
			want:      networks.ErrInvalidRetrieval,
		},
		"a report carrying no payload": {
			retrieval: evidenceTestRetrieval(t),
			report:    noPayload,
			want:      networks.ErrMissingRawPayload,
		},
		"a report carrying a status nobody mapped": {
			retrieval: evidenceTestRetrieval(t),
			report:    unmapped,
			want:      networks.ErrUnmappableStatus,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fake := &evidenceTestStore{}
			writer, err := networks.NewEvidenceWriter(fake)
			if err != nil {
				t.Fatalf("NewEvidenceWriter(): %v", err)
			}
			if _, err := writer.Record(t.Context(), c.retrieval, c.report); !errors.Is(err, c.want) {
				t.Fatalf("Record() = %v, want one wrapping %v", err, c.want)
			}
			if len(fake.got) != 0 {
				t.Errorf("the writer sent %d insert(s) for a value it refused; the row would be permanent", len(fake.got))
			}
		})
	}
}

// TestRecordCarriesTheDatabasesRefusalThrough keeps the cause intact. The
// ingestion path reads it to tell an unchanged re-report (T053) and a
// superseding one (T054) apart from a genuine failure, and a writer that
// flattened them into one message would make that impossible.
func TestRecordCarriesTheDatabasesRefusalThrough(t *testing.T) {
	t.Parallel()

	refused := errors.New("ERROR: duplicate key value violates unique constraint \"network_transaction_unique_report\"")
	writer, err := networks.NewEvidenceWriter(&evidenceTestStore{fail: refused})
	if err != nil {
		t.Fatalf("NewEvidenceWriter(): %v", err)
	}

	_, err = writer.Record(t.Context(), evidenceTestRetrieval(t), evidenceTestReport(t))
	if !errors.Is(err, networks.ErrEvidenceNotWritten) {
		t.Fatalf("Record() = %v, want one wrapping ErrEvidenceNotWritten", err)
	}
	if !errors.Is(err, refused) {
		t.Errorf("Record() = %v, which does not carry the database's own error; the dedup and supersede paths read it", err)
	}
	if !strings.Contains(err.Error(), "FIX-1001") {
		t.Errorf("the refusal %q does not name the transaction it was about", err)
	}
}

// TestTheWriterCannotSupplyADigest is a structural rule rather than a
// behavioural one, and it is here because the behaviour it protects is
// unobservable: a digest sent to a column the trigger overwrites looks
// exactly like one that was never sent. The query names no such column, so
// the generated parameters have no such field, and this fails the day
// somebody adds one.
func TestTheWriterCannotSupplyADigest(t *testing.T) {
	t.Parallel()

	params := reflect.TypeOf(store.InsertNetworkTransactionParams{})
	if _, found := params.FieldByName("ContentDigest"); found {
		t.Error("InsertNetworkTransactionParams carries a ContentDigest field; the database computes the digest from the reported facts and discards a caller's, so a writer that appears to supply one is claiming an authority it does not have over the fingerprint of its own evidence")
	}
	if params.NumField() == 0 {
		t.Fatal("the parameters carry no fields at all, so this rule judged nothing")
	}
}

// TestRecordAgainstTheRealSchema is the layer no fake can stand in for: the
// row the writer builds either stores or it does not, and only the migrated
// database knows which.
func TestRecordAgainstTheRealSchema(t *testing.T) {
	t.Parallel()

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the evidence writer")
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

	// The account the writer will file under has to be a row: network_account_id
	// is a foreign key, and a writer that could file under an account nobody
	// configured would produce evidence attributable to nothing.
	networkID := "fixture_evidence"
	if _, err := tx.Exec(ctx, `
		insert into cashback.network (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_minute, active)
		values ($1, 'Evidence Network', 'clickref', 31, 360, true)`, networkID); err != nil {
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
	report := evidenceTestReport(t)

	writer, err := networks.NewEvidenceWriter(store.New(tx))
	if err != nil {
		t.Fatalf("NewEvidenceWriter(): %v", err)
	}
	recorded, err := writer.Record(ctx, retrieval, report)
	if err != nil {
		t.Fatalf("Record(): %v", err)
	}
	if recorded.ID == uuid.Nil {
		t.Fatal("the stored row has no identity")
	}
	if len(recorded.ContentDigest) != 64 {
		t.Errorf("the database returned a digest of %d characters, want 64 hex characters of sha256", len(recorded.ContentDigest))
	}
	if !recorded.RetrievedAt.Equal(retrieval.RetrievedAt) {
		t.Errorf("the row holds retrieved_at %s, want the %s the retrieval stated", recorded.RetrievedAt, retrieval.RetrievedAt)
	}

	// And it really is the row: read back through the store rather than
	// trusting what the insert returned.
	stored, err := store.New(tx).GetNetworkTransaction(ctx, pgtype.UUID{Bytes: recorded.ID, Valid: true})
	if err != nil {
		t.Fatalf("GetNetworkTransaction(): %v", err)
	}
	if stored.ExternalID != report.ExternalID || stored.ContentDigest != recorded.ContentDigest {
		t.Errorf("stored %s/%s, want %s/%s", stored.ExternalID, stored.ContentDigest, report.ExternalID, recorded.ContentDigest)
	}
	if stored.SupersedesID.Valid {
		t.Error("the first report was stored naming a predecessor")
	}

	// An unattributed report is evidence too (FR-034), and it is the case a
	// nullable column exists for.
	unattributed := evidenceTestReport(t)
	unattributed.ExternalID = "FIX-1002"
	unattributed.ClickRef = networks.ClickRef{}
	if _, err := writer.Record(ctx, retrieval, unattributed); err != nil {
		t.Fatalf("a transaction the network reported with no click reference was refused: %v", err)
	}
}
