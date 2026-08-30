// The tests for poller.go that stop short of a database: what a poll refuses
// before it opens a transaction, and the adapter every other poller test is
// driven through.
//
// The refusals are here rather than beside the schema cases because the
// property being asserted is that NOTHING HAPPENED - no transaction, no
// fetch - and a test that had a database to look at would be tempted to
// prove that by finding no rows, which a poll that rolled back would satisfy
// just as well.

package networks_test

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// pollerTestAdapter answers from a script rather than from a recording: each
// case says what the network reports in the window the poller asks for.
//
// It is a fake rather than the fixture adapter next door because what is
// under test is the POLLER - which window it asks for, what it does with the
// answer, when the cursor moves - and the fixture's recorded lifecycle would
// decide half of that. The fixture gets its own test, at the end, where the
// whole chain is the point.
type pollerTestAdapter struct {
	id      networks.NetworkID
	account networks.PublisherAccount
	limits  networks.Limits
	// answer is called once per fetch with the call's index and the window
	// the poller chose. Reports come back first and the error after them,
	// yielded rather than returned, because that is how a real adapter
	// reports a network that failed part way through a window (contract
	// rules 8 and 9).
	answer func(call int, window networks.QueryWindow) ([]networks.Reported, error)
	// windows is every window this adapter was asked for, in order. It is
	// what a case reads to assert the poller never skipped a period and
	// never asked for one twice.
	windows []networks.QueryWindow
}

func pollerTestNetwork(account networks.PublisherAccount, answer func(int, networks.QueryWindow) ([]networks.Reported, error)) *pollerTestAdapter {
	return &pollerTestAdapter{
		id:      account.Network(),
		account: account,
		limits:  networks.Limits{MaxWindow: 31 * 24 * time.Hour, RequestsPerSecond: 6},
		answer:  answer,
	}
}

func (a *pollerTestAdapter) ID() networks.NetworkID             { return a.id }
func (a *pollerTestAdapter) Account() networks.PublisherAccount { return a.account }
func (a *pollerTestAdapter) Limits() networks.Limits            { return a.limits }

func (a *pollerTestAdapter) BuildDeeplink(context.Context, networks.DeeplinkTarget, networks.IssuedClickRef) (string, error) {
	return "", fmt.Errorf("%w: this adapter exists to be polled", networks.ErrDeeplinkNotFormed)
}

func (a *pollerTestAdapter) FetchCatalogue(context.Context) (iter.Seq2[networks.ReportedMerchant, error], error) {
	return nil, errors.New("this adapter exists to be polled")
}

// FetchTransactions records the window and answers from the script. It holds
// itself to its own declared limits first, exactly as contract rule 3
// requires - which is what makes "the poller never asks for more than the
// network allows" a property this fake can actually refuse.
func (a *pollerTestAdapter) FetchTransactions(_ context.Context, window networks.QueryWindow) (iter.Seq2[networks.Reported, error], error) {
	if err := a.limits.ValidateWindow(window); err != nil {
		return nil, fmt.Errorf("pollerTestAdapter: %w", err)
	}
	a.windows = append(a.windows, window)
	call := len(a.windows) - 1
	return func(yield func(networks.Reported, error) bool) {
		reports, failed := a.answer(call, window)
		for _, report := range reports {
			if !yield(report, nil) {
				return
			}
		}
		if failed != nil {
			yield(networks.Reported{}, failed)
		}
	}, nil
}

// pollerTestNothing is the script for a case that never gets as far as a
// read: the network is not asked, so what it would have answered is beside
// the point.
func pollerTestNothing(int, networks.QueryWindow) ([]networks.Reported, error) { return nil, nil }

// pollerTestDB counts what it was asked to begin, and can refuse. Every case
// in this file expects the count to stay at zero.
type pollerTestDB struct {
	begun int
	fail  error
}

func (d *pollerTestDB) Begin(context.Context) (pgx.Tx, error) {
	d.begun++
	if d.fail != nil {
		return nil, d.fail
	}
	// No case here gets this far. A poll that did would panic on the nil
	// transaction rather than quietly do something with it, which is the
	// louder of the two failures.
	return nil, nil
}

func pollerTestAccount(t *testing.T) networks.PublisherAccount {
	t.Helper()
	account, err := networks.NewPublisherAccount(uuid.New(), networks.NetworkID("fixture"), "publisher-1")
	if err != nil {
		t.Fatalf("NewPublisherAccount(): %v", err)
	}
	return account
}

// TestNewPollerRefusesWhatItCannotPollWith is now a short test, and the
// shortness is the point: a poller needs somewhere to write and nothing
// else. Where an account starts reading is a fact about that ACCOUNT and
// lives on its row (0023), so one poller serves every account this process
// polls and cannot be given the wrong one's start.
func TestNewPollerRefusesWhatItCannotPollWith(t *testing.T) {
	t.Parallel()

	if _, err := networks.NewPoller(nil); !errors.Is(err, networks.ErrNoPollerStore) {
		t.Errorf("NewPoller(nil) = %v, want one wrapping ErrNoPollerStore", err)
	}
	if _, err := networks.NewPoller(&pollerTestDB{}); err != nil {
		t.Errorf("NewPoller() refused a usable poller: %v", err)
	}
}

// TestPollRefusesBeforeItOpensATransaction is the wiring check, run again at
// every poll. Each of these is otherwise found out AFTER a window has been
// fetched - and the last of them is the one that never surfaces at all: an
// adapter with no declared limits makes every window empty, so the poll
// reports success, stores nothing, moves the cursor nowhere, and is run
// again forever with no error anywhere to say why.
func TestPollRefusesBeforeItOpensATransaction(t *testing.T) {
	t.Parallel()

	account := pollerTestAccount(t)
	noLimits := pollerTestNetwork(account, pollerTestNothing)
	noLimits.limits = networks.Limits{}
	elsewhere := pollerTestNetwork(account, pollerTestNothing)
	elsewhere.id = networks.NetworkID("another")
	nobody := pollerTestNetwork(networks.PublisherAccount{}, pollerTestNothing)
	nobody.id = account.Network()
	nameless := pollerTestNetwork(account, pollerTestNothing)
	nameless.id = networks.NetworkID("")

	cases := map[string]struct {
		adapter networks.Network
		want    error
	}{
		"no adapter at all": {
			adapter: nil, want: networks.ErrNoPollerStore,
		},
		"an adapter that names no network": {
			adapter: nameless, want: networks.ErrInvalidNetworkID,
		},
		"an adapter speaking for nobody": {
			adapter: nobody, want: networks.ErrInvalidPublisherAccount,
		},
		"an adapter polling an account at another network": {
			adapter: elsewhere, want: networks.ErrInvalidPublisherAccount,
		},
		"an adapter that declares no limits": {
			adapter: noLimits, want: networks.ErrInvalidLimits,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			db := &pollerTestDB{}
			poller, err := networks.NewPoller(db)
			if err != nil {
				t.Fatalf("NewPoller(): %v", err)
			}

			forward, err := poller.PollForward(t.Context(), tc.adapter)
			if !errors.Is(err, tc.want) {
				t.Fatalf("PollForward() = %v, want one wrapping %v", err, tc.want)
			}
			if forward.Ran {
				t.Error("a refused poll reported that it ran")
			}
			if _, err := poller.PollTrailing(t.Context(), tc.adapter); !errors.Is(err, tc.want) {
				t.Errorf("PollTrailing() = %v, want one wrapping %v", err, tc.want)
			}
			if db.begun != 0 {
				t.Errorf("a refused poll opened %d transaction(s); the refusal is worth having only if it is free", db.begun)
			}
		})
	}
}

// TestPollReportsAFailureToBegin names the account. A scheduler polling
// twenty accounts logs twenty failures, and one that said only "begin
// failed" would leave an operator to guess which network stopped ingesting.
func TestPollReportsAFailureToBegin(t *testing.T) {
	t.Parallel()

	broken := errors.New("connection refused")
	account := pollerTestAccount(t)
	poller, err := networks.NewPoller(&pollerTestDB{fail: broken})
	if err != nil {
		t.Fatalf("NewPoller(): %v", err)
	}

	_, err = poller.PollForward(t.Context(), pollerTestNetwork(account, pollerTestNothing))
	if !errors.Is(err, broken) {
		t.Fatalf("PollForward() = %v, want one wrapping the cause", err)
	}
	if !strings.Contains(err.Error(), account.String()) {
		t.Errorf("PollForward() = %q, want it to name %s", err, account)
	}
}
