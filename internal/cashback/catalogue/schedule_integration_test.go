// The tests for schedule.go: what a scheduled import commits, and what it
// must leave alone (T105).
//
// Against the real schema, because the claim is about a TRANSACTION - all of
// the import or none of it - and a fake store would agree with the code
// rather than with Postgres. The outer transaction each case runs in doubles
// as the isolation: pgx opens a savepoint for the import's own transaction,
// so a commit inside is visible to the case and still thrown away with it.

package catalogue_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue"
	"github.com/Nomos-N4s/apivo-news/internal/platform/scheduler"
)

// scheduledImport builds the job over a transaction, running at one instant.
//
// The instant is a parameter and not a constant because the import's
// reconciliation compares what it just stamped against what was stamped
// before: two runs at the same instant leave nothing to withdraw, which
// would make a departure test pass for the wrong reason.
func scheduledImport(t *testing.T, tx pgx.Tx, net *stubNetwork, at time.Time) *catalogue.Imports {
	t.Helper()
	imports, err := catalogue.NewImports(discard(), tx, net, anImporter(t, at))
	if err != nil {
		t.Fatalf("NewImports(): %v", err)
	}
	return imports
}

// routeCount is how many routes exist at one network.
func routeCount(ctx context.Context, t *testing.T, tx pgx.Tx, networkID string) int {
	t.Helper()
	var n int
	if err := tx.QueryRow(ctx,
		`select count(*) from cashback.merchant_network where network_id = $1`, networkID).Scan(&n); err != nil {
		t.Fatalf("counting routes: %v", err)
	}
	return n
}

// TestAScheduledImportCommitsTheWholeCatalogue is the ordinary run: the
// retailers the network returned are there afterwards.
func TestAScheduledImportCommitsTheWholeCatalogue(t *testing.T) {
	ctx, tx, net := importTestTx(t)
	net.merchants = []networks.ReportedMerchant{
		aReportedMerchant(t, "5001", "Gartenhaus", "DE", networks.MerchantStatusActive),
		aReportedMerchant(t, "5002", "Buchladen", "DE", networks.MerchantStatusActive),
	}

	if err := scheduledImport(t, tx, net, importTestAt).Refresh(ctx); err != nil {
		t.Fatalf("Refresh(): %v", err)
	}
	if got := routeCount(ctx, t, tx, net.id.String()); got != 2 {
		t.Errorf("the import committed %d routes, want 2", got)
	}
}

// TestAnImportThatCouldNotFinishCommitsNothing is the whole reason the run
// is one transaction. The import's last act withdraws every route the
// network did not return; a half-written run that reconciled anyway would
// stop publishing retailers on the strength of a read that never finished.
func TestAnImportThatCouldNotFinishCommitsNothing(t *testing.T) {
	ctx, tx, net := importTestTx(t)
	// A catalogue already imported, so there is something to lose.
	net.merchants = []networks.ReportedMerchant{
		aReportedMerchant(t, "6001", "Gartenhaus", "DE", networks.MerchantStatusActive),
		aReportedMerchant(t, "6002", "Buchladen", "DE", networks.MerchantStatusActive),
	}
	if err := scheduledImport(t, tx, net, importTestAt).Refresh(ctx); err != nil {
		t.Fatalf("seeding the first import: %v", err)
	}
	before := routeCount(ctx, t, tx, net.id.String())

	// The next run dies after the first retailer.
	net.merchants = []networks.ReportedMerchant{
		aReportedMerchant(t, "6003", "Neuer Laden", "DE", networks.MerchantStatusActive),
		aReportedMerchant(t, "6004", "Noch einer", "DE", networks.MerchantStatusActive),
	}
	net.failAfter = 1

	err := scheduledImport(t, tx, net, importTestAt).Refresh(ctx)
	if !errors.Is(err, catalogue.ErrNotImported) {
		t.Fatalf("Refresh() error = %v, want ErrNotImported", err)
	}
	if got := routeCount(ctx, t, tx, net.id.String()); got != before {
		t.Errorf("a failed import left %d routes where there were %d; nothing should have been written", got, before)
	}
	for _, external := range []string{"6001", "6002"} {
		status, found := routeStatus(ctx, t, tx, net.id.String(), external)
		if !found {
			t.Errorf("route %s is gone after a failed import", external)
			continue
		}
		if status != "active" {
			t.Errorf("route %s is %q after a failed import, want active", external, status)
		}
	}
}

// TestANetworkThatWillNotAnswerIsNotACatalogueChange. The read failed before
// a single retailer arrived, which says nothing at all about who is still
// live - and the departure sweep must not run on it.
func TestANetworkThatWillNotAnswerIsNotACatalogueChange(t *testing.T) {
	ctx, tx, net := importTestTx(t)
	net.merchants = []networks.ReportedMerchant{
		aReportedMerchant(t, "7001", "Gartenhaus", "DE", networks.MerchantStatusActive),
	}
	if err := scheduledImport(t, tx, net, importTestAt).Refresh(ctx); err != nil {
		t.Fatalf("seeding the first import: %v", err)
	}

	net.immediate = networks.ErrNetworkUnavailable
	if err := scheduledImport(t, tx, net, importTestAt).Refresh(ctx); !errors.Is(err, catalogue.ErrNotImported) {
		t.Fatalf("Refresh() error = %v, want ErrNotImported", err)
	}
	if status, found := routeStatus(ctx, t, tx, net.id.String(), "7001"); !found || status != "active" {
		t.Errorf("route 7001 is %q (found=%t) after a network that would not answer, want active", status, found)
	}
}

// TestADepartedRetailerStopsBeingPublished is the other half of what the
// import is for, run through the job rather than the importer: a retailer
// the network stopped listing is withdrawn.
func TestADepartedRetailerStopsBeingPublished(t *testing.T) {
	ctx, tx, net := importTestTx(t)
	net.merchants = []networks.ReportedMerchant{
		aReportedMerchant(t, "8001", "Bleibt", "DE", networks.MerchantStatusActive),
		aReportedMerchant(t, "8002", "Geht", "DE", networks.MerchantStatusActive),
	}
	if err := scheduledImport(t, tx, net, importTestAt).Refresh(ctx); err != nil {
		t.Fatalf("seeding the first import: %v", err)
	}

	// An hour later, so the second run's stamp is strictly after the first
	// one's and the routes it did not touch are visibly stale.
	net.merchants = net.merchants[:1]
	if err := scheduledImport(t, tx, net, importTestAt.Add(time.Hour)).Refresh(ctx); err != nil {
		t.Fatalf("Refresh(): %v", err)
	}

	if status, _ := routeStatus(ctx, t, tx, net.id.String(), "8001"); status != "active" {
		t.Errorf("the retailer the network still lists is %q, want active", status)
	}
	if status, _ := routeStatus(ctx, t, tx, net.id.String(), "8002"); status != "left_network" {
		t.Errorf("the retailer the network stopped listing is %q, want left_network", status)
	}
}

// TestAnImportThatOnlyWithdrawsSaysSo. Withdrawing retailers and adding none
// is what a credential that quietly lost its programme approvals looks like
// from here, and it is indistinguishable from the world having changed until
// somebody is told. Saying so at WARN is that somebody being told.
func TestAnImportThatOnlyWithdrawsSaysSo(t *testing.T) {
	ctx, tx, net := importTestTx(t)
	net.merchants = []networks.ReportedMerchant{
		aReportedMerchant(t, "8101", "Bleibt", "DE", networks.MerchantStatusActive),
		aReportedMerchant(t, "8102", "Geht", "DE", networks.MerchantStatusActive),
	}
	if err := scheduledImport(t, tx, net, importTestAt).Refresh(ctx); err != nil {
		t.Fatalf("seeding the first import: %v", err)
	}

	var said strings.Builder
	loud, err := catalogue.NewImports(
		slog.New(slog.NewTextHandler(&said, &slog.HandlerOptions{Level: slog.LevelWarn})),
		tx, net, anImporter(t, importTestAt.Add(time.Hour)))
	if err != nil {
		t.Fatalf("NewImports(): %v", err)
	}
	net.merchants = net.merchants[:1]
	if err := loud.Refresh(ctx); err != nil {
		t.Fatalf("Refresh(): %v", err)
	}
	if !strings.Contains(said.String(), "withdrew retailers and added none") {
		t.Errorf("the import said nothing about withdrawing retailers and adding none: %s", said.String())
	}
}

// TestTheImportIsRegisteredUnderItsOwnName. The name is what the fleet-wide
// lock is taken on, so a second registration under it must be refused rather
// than quietly giving two jobs one lock.
func TestTheImportIsRegisteredUnderItsOwnName(t *testing.T) {
	ctx, tx, net := importTestTx(t)
	_ = ctx

	jobs := scheduler.New(discard(), nil, scheduler.Config{})
	if err := scheduledImport(t, tx, net, importTestAt).Register(jobs); err != nil {
		t.Fatalf("Register(): %v", err)
	}
	if err := scheduledImport(t, tx, net, importTestAt).Register(jobs); err == nil {
		t.Error("a second import registered under the same name, so two jobs would share one lock")
	}
}

// TestAnImportNeedsEveryOneOfItsParts. Each is a wiring mistake the
// composition root can make, and each is cheap to find there.
func TestAnImportNeedsEveryOneOfItsParts(t *testing.T) {
	ctx, tx, net := importTestTx(t)
	_ = ctx
	importer := anImporter(t, importTestAt)

	for name, one := range map[string]struct {
		log      bool
		db       catalogue.Beginner
		adapter  *stubNetwork
		importer *catalogue.Importer
	}{
		"no logger":   {false, tx, net, importer},
		"no database": {true, nil, net, importer},
		"no network":  {true, tx, nil, importer},
		"no importer": {true, tx, net, nil},
	} {
		var log = discard()
		if !one.log {
			log = nil
		}
		// A typed nil adapter would satisfy the interface and defeat the
		// check, so the nil case passes a genuinely absent one.
		if one.adapter == nil {
			if _, err := catalogue.NewImports(log, one.db, nil, one.importer); err == nil {
				t.Errorf("%s: NewImports() succeeded, want a refusal", name)
			}
			continue
		}
		if _, err := catalogue.NewImports(log, one.db, one.adapter, one.importer); err == nil {
			t.Errorf("%s: NewImports() succeeded, want a refusal", name)
		}
	}
}

// breakingDB is the transaction the case runs in, with one of the two
// transaction verbs made to fail.
//
// Both failures are real and neither is reachable from a stub network: a
// pool that cannot open a transaction, and a commit refused at the end of a
// long one - a connection dropped, a disk full, a server restarted. What
// matters is that the job reports them as an import that did not happen
// rather than logging a catalogue it did not write.
type breakingDB struct {
	pgx.Tx
	onBegin  error
	onCommit error
}

func (b *breakingDB) Begin(ctx context.Context) (pgx.Tx, error) {
	if b.onBegin != nil {
		return nil, b.onBegin
	}
	inner, err := b.Tx.Begin(ctx)
	if err != nil || b.onCommit == nil {
		return inner, err
	}
	return brokenCommit{Tx: inner, err: b.onCommit}, nil
}

// brokenCommit is a transaction that refuses to commit and rolls back
// normally, so the case's own transaction is still clean afterwards.
type brokenCommit struct {
	pgx.Tx
	err error
}

func (b brokenCommit) Commit(context.Context) error { return b.err }

// TestATransactionThatFailsIsAnImportThatDidNotHappen. A commit refused at
// the end of a long import is the one failure that arrives after every row
// has been written and every retailer reconciled - and reporting it as a
// success would log a catalogue nobody has.
func TestATransactionThatFailsIsAnImportThatDidNotHappen(t *testing.T) {
	ctx, tx, net := importTestTx(t)
	net.merchants = []networks.ReportedMerchant{
		aReportedMerchant(t, "9001", "Gartenhaus", "DE", networks.MerchantStatusActive),
	}
	broken := errors.New("the connection went away")

	for name, db := range map[string]*breakingDB{
		"the transaction never opened": {Tx: tx, onBegin: broken},
		"the commit was refused":       {Tx: tx, onCommit: broken},
	} {
		imports, err := catalogue.NewImports(discard(), db, net, anImporter(t, importTestAt))
		if err != nil {
			t.Fatalf("%s: NewImports(): %v", name, err)
		}
		err = imports.Refresh(ctx)
		if !errors.Is(err, catalogue.ErrNotImported) {
			t.Errorf("%s: Refresh() error = %v, want ErrNotImported", name, err)
		}
		if !errors.Is(err, broken) {
			t.Errorf("%s: Refresh() error = %v, which does not carry what actually failed", name, err)
		}
	}
	if got := routeCount(ctx, t, tx, net.id.String()); got != 0 {
		t.Errorf("an import that never committed left %d routes behind", got)
	}
}
