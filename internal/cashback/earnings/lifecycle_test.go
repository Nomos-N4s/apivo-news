package earnings_test

// What the crediting job refuses at construction, and what it does when the
// database cannot be read (#435). Everything it does when the database CAN
// be read is lifecycle_integration_test.go's, against the real schema.

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings"
	walletmemory "github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/memory"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// unreachableDB is a database every read of which fails, so a run finds
// nothing it can act on and has to say so.
type unreachableDB struct{}

var errUnreachable = errors.New("the database is unreachable")

func (unreachableDB) Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errUnreachable
}

func (unreachableDB) Query(context.Context, string, ...interface{}) (pgx.Rows, error) {
	return nil, errUnreachable
}

func (unreachableDB) QueryRow(context.Context, string, ...interface{}) pgx.Row {
	return unreachableRow{}
}

func (unreachableDB) Begin(context.Context) (pgx.Tx, error) { return nil, errUnreachable }

type unreachableRow struct{}

func (unreachableRow) Scan(...any) error { return errUnreachable }

func TestALifecycleNeedsEveryOneOfItsParts(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.DiscardHandler)
	for name, build := range map[string]func() (*earnings.Lifecycle, error){
		"no logger": func() (*earnings.Lifecycle, error) {
			return earnings.NewLifecycle(nil, unreachableDB{}, walletmemory.New(), "receivable", earnings.HoldRules{})
		},
		"no database": func() (*earnings.Lifecycle, error) {
			return earnings.NewLifecycle(log, nil, walletmemory.New(), "receivable", earnings.HoldRules{})
		},
		"no ledger": func() (*earnings.Lifecycle, error) {
			return earnings.NewLifecycle(log, unreachableDB{}, nil, "receivable", earnings.HoldRules{})
		},
		"no receivable": func() (*earnings.Lifecycle, error) {
			return earnings.NewLifecycle(log, unreachableDB{}, walletmemory.New(), "", earnings.HoldRules{})
		},
		"rules that cannot run": func() (*earnings.Lifecycle, error) {
			return earnings.NewLifecycle(log, unreachableDB{}, walletmemory.New(), "receivable",
				earnings.HoldRules{MemberVelocity: 3})
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			job, err := build()
			if !errors.Is(err, earnings.ErrLifecycleNotConfigured) {
				t.Fatalf("error = %v, want one wrapping %v", err, earnings.ErrLifecycleNotConfigured)
			}
			if job != nil {
				t.Error("a refused job was still returned")
			}
		})
	}
}

func TestTheJobKeepsTheRulesItWasGiven(t *testing.T) {
	t.Parallel()
	rules := earnings.HoldRules{
		SharedContextAccounts: 2, SharedContextWindow: 24 * time.Hour,
		SaleCap: money.Amount{Minor: 100_000, Currency: "EUR"},
	}
	job, err := earnings.NewLifecycle(slog.New(slog.DiscardHandler), unreachableDB{}, walletmemory.New(), "receivable", rules)
	if err != nil {
		t.Fatalf("NewLifecycle(): %v", err)
	}
	if got := job.Rules(); got != rules {
		t.Errorf("Rules() = %+v, want %+v", got, rules)
	}
}

// TestARunThatCannotReadStopsAndSaysSo. A pass that cannot see what is
// waiting cannot act on it, and the scheduler's log line has to say the run
// was not clean rather than "ran, nothing to do".
func TestARunThatCannotReadStopsAndSaysSo(t *testing.T) {
	t.Parallel()
	job, err := earnings.NewLifecycle(slog.New(slog.DiscardHandler), unreachableDB{}, walletmemory.New(), "receivable", earnings.HoldRules{})
	if err != nil {
		t.Fatalf("NewLifecycle(): %v", err)
	}
	out, err := job.Run(context.Background())
	if !errors.Is(err, earnings.ErrLifecycleIncomplete) || !errors.Is(err, errUnreachable) {
		t.Fatalf("Run() error = %v, want one wrapping %v and the read's own %v", err, earnings.ErrLifecycleIncomplete, errUnreachable)
	}
	if out != (earnings.Outcome{}) {
		t.Errorf("a run that read nothing reports %+v, want nothing done", out)
	}
}
