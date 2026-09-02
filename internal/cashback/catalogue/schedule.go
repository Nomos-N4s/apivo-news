// Running the import (T105, FR-012).
//
// [Importer] writes a catalogue; nothing was asking it to. A deployment
// therefore started with whatever retailers were in the database when it was
// last seeded, and never learned about a new one, a departed one, or a
// changed name - and the failure is silent, because an empty or stale
// catalogue looks exactly like a catalogue.
//
// This is the thing that asks. One job, on a schedule, holding the
// fleet-wide lock so two instances never import the same network at once.

package catalogue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue/store"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/platform/scheduler"
)

const (
	// ImportJobName identifies the import in the scheduler, and names the
	// fleet-wide lock that stops two instances importing at once. Two
	// concurrent imports would each stamp their own start instant and then
	// reconcile against it, so the slower one would mark everything the
	// faster one had just written as departed.
	ImportJobName = "cashback-catalogue-import"
	// ImportInterval is how stale a catalogue may get. Networks publish
	// advertiser lists daily at most, so this is generous - but it is hours
	// rather than a day so that a fresh deployment has a catalogue within
	// the first shift rather than the first night, and so a retailer who
	// left this morning stops being clickable this afternoon.
	ImportInterval = 6 * time.Hour
	// importTimeout bounds one run. A whole catalogue is thousands of
	// retailers over a paginated API, so the bound is generous - but it
	// exists, because a network that stops answering mid-page must not hold
	// the lock until the process dies.
	importTimeout = 15 * time.Minute
)

// ErrNotImported reports a scheduled import that did not complete. Every
// failure below wraps it, so the job's log line reads the same whether the
// network, the transaction or the mapping was what failed.
var ErrNotImported = errors.New("catalogue: the import could not be completed")

// Beginner opens the transaction one import runs in. Defined here per the
// boundary rules - the consumer names its dependency - and satisfied by
// *pgxpool.Pool.
type Beginner interface {
	store.DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Imports runs one network's catalogue import on a schedule.
type Imports struct {
	log      *slog.Logger
	db       Beginner
	adapter  networks.Network
	importer *Importer
}

// NewImports assembles the job, refusing one that could not run.
func NewImports(log *slog.Logger, db Beginner, adapter networks.Network, importer *Importer) (*Imports, error) {
	switch {
	case log == nil:
		return nil, fmt.Errorf("%w: no logger, and an import nobody hears about is an import nobody fixes", ErrImportNotConfigured)
	case db == nil:
		return nil, fmt.Errorf("%w: no database to import into", ErrImportNotConfigured)
	case adapter == nil:
		return nil, fmt.Errorf("%w: no network to import from", ErrImportNotConfigured)
	case importer == nil:
		return nil, fmt.Errorf("%w: nothing to write the catalogue with", ErrImportNotConfigured)
	}
	return &Imports{log: log, db: db, adapter: adapter, importer: importer}, nil
}

// Register puts the import on the scheduler.
func (i *Imports) Register(jobs *scheduler.Scheduler) error {
	return jobs.Register(scheduler.Job{
		Name:     ImportJobName,
		Interval: ImportInterval,
		Timeout:  importTimeout,
		Run:      i.Refresh,
	})
}

// Refresh imports the whole catalogue in ONE transaction.
//
// One transaction, and that is the point rather than a detail. The import's
// last act is to mark every route the network did not return as departed,
// which is what makes a retailer leaving a network stop being clickable. A
// run that wrote four thousand retailers, reconciled, and then failed to
// commit half of it would leave a catalogue that is part new and part old,
// with the departure sweep applied across both - retailers withdrawn on the
// strength of a read that was never finished.
//
// So it is all of it or none of it. The cost is one long transaction every
// few hours; the alternative is a catalogue nobody can reason about after a
// crash.
func (i *Imports) Refresh(ctx context.Context) error {
	tx, err := i.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%w: opening the transaction: %w", ErrNotImported, err)
	}
	// Rollback after a commit is a no-op, so this is the one path that runs
	// on every exit and the only one that has to.
	defer func() { _ = tx.Rollback(ctx) }()

	result, err := i.importer.Run(ctx, tx, i.adapter)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotImported, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: committing %d retailers: %w", ErrNotImported, result.Seen, err)
	}

	i.log.InfoContext(ctx, "catalogue imported",
		"network", i.adapter.ID().String(),
		"seen", result.Seen,
		"created", result.Created,
		"departed", result.Departed,
		"started_at", result.StartedAt)
	// A run that withdrew retailers and added none is what an upstream
	// change looks like from here - a credential that lost its programme
	// approvals, a filter somebody edited - and it is indistinguishable
	// from the world having changed until somebody looks. Saying so at WARN
	// is that somebody being told.
	if result.Departed > 0 && result.Created == 0 {
		i.log.WarnContext(ctx, "the catalogue import withdrew retailers and added none",
			"network", i.adapter.ID().String(),
			"departed", result.Departed,
			"seen", result.Seen)
	}
	return nil
}
