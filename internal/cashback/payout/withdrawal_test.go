package payout_test

// What a withdrawal service refuses before it is ever asked for anything,
// and what its refusals say. No database: these are the mistakes that are
// wiring, and the sentences a member reads.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/memory"
)

// TestAServiceMissingAPartIsRefusedAtConstruction. Discovered later, each of
// these is discovered inside a request that has already begun moving money.
func TestAServiceMissingAPartIsRefusedAtConstruction(t *testing.T) {
	threshold := euro(t, 2500)
	if _, err := payout.NewWithdrawals(nil, memory.New(), "network-receivable", threshold); !errors.Is(err, payout.ErrNoWithdrawalStore) {
		t.Errorf("with no database = %v, want one wrapping %v", err, payout.ErrNoWithdrawalStore)
	}
	if _, err := payout.NewWithdrawals(noDatabase{}, nil, "network-receivable", threshold); !errors.Is(err, payout.ErrNoLedger) {
		t.Errorf("with no ledger = %v, want one wrapping %v", err, payout.ErrNoLedger)
	}
}

// TestADeploymentThatNamedNoReceivableAnswersOnTheEndpoint. It is not
// refused at construction: doing so would take the wallet, the click-out and
// the operator queue down over a deployment that simply cannot pay out yet.
// Production refuses to start without the key; here the endpoint says so.
func TestADeploymentThatNamedNoReceivableAnswersOnTheEndpoint(t *testing.T) {
	withdrawals, err := payout.NewWithdrawals(noDatabase{}, memory.New(), "", euro(t, 2500))
	if err != nil {
		t.Fatalf("NewWithdrawals() with no receivable = %v, want it built", err)
	}
	if _, err := withdrawals.Request(t.Context(), payout.Request{
		Member: uuid.New(), Destination: uuid.New(), Amount: euro(t, 5000),
	}); !errors.Is(err, payout.ErrNoReceivable) {
		t.Errorf("Request() = %v, want one wrapping %v", err, payout.ErrNoReceivable)
	}
}

// TestARequestNamingNobodyIsRefused. Both come from the caller rather than
// from a member, so both are programming mistakes - and neither reaches a
// transaction.
func TestARequestNamingNobodyIsRefused(t *testing.T) {
	withdrawals, err := payout.NewWithdrawals(noDatabase{}, memory.New(), "network-receivable", euro(t, 2500))
	if err != nil {
		t.Fatalf("NewWithdrawals(): %v", err)
	}
	amount := euro(t, 5000)

	if _, err := withdrawals.Request(t.Context(), payout.Request{
		Destination: uuid.New(), Amount: amount,
	}); !errors.Is(err, payout.ErrNotRequested) {
		t.Errorf("with no member = %v, want one wrapping %v", err, payout.ErrNotRequested)
	}
	if _, err := withdrawals.Request(t.Context(), payout.Request{
		Member: uuid.New(), Amount: amount,
	}); !errors.Is(err, payout.ErrNotRequested) {
		t.Errorf("with no destination = %v, want one wrapping %v", err, payout.ErrNotRequested)
	}
}

// TestAWithdrawalReadForNobodyIsRefused. Get and List take the member as
// well as the id, so there is no call either could make without it.
func TestAWithdrawalReadForNobodyIsRefused(t *testing.T) {
	withdrawals, err := payout.NewWithdrawals(noDatabase{}, memory.New(), "network-receivable", euro(t, 2500))
	if err != nil {
		t.Fatalf("NewWithdrawals(): %v", err)
	}

	if _, err := withdrawals.Get(t.Context(), uuid.Nil, uuid.New()); !errors.Is(err, payout.ErrWithdrawalNotFound) {
		t.Errorf("Get(no member) = %v, want one wrapping %v", err, payout.ErrWithdrawalNotFound)
	}
	if _, err := withdrawals.Get(t.Context(), uuid.New(), uuid.Nil); !errors.Is(err, payout.ErrWithdrawalNotFound) {
		t.Errorf("Get(no id) = %v, want one wrapping %v", err, payout.ErrWithdrawalNotFound)
	}
	if _, err := withdrawals.List(t.Context(), uuid.Nil); !errors.Is(err, payout.ErrWithdrawalNotFound) {
		t.Errorf("List(no member) = %v, want one wrapping %v", err, payout.ErrWithdrawalNotFound)
	}
}

// TestBelowThresholdSaysAllThreeFigures. "Insufficient" without them is a
// message somebody has to reproduce to act on.
func TestBelowThresholdSaysAllThreeFigures(t *testing.T) {
	err := payout.BelowThreshold{
		Threshold: euro(t, 2500),
		Confirmed: euro(t, 1500),
		Short:     euro(t, 1000),
	}
	// Minor units, as money.Amount.String renders them: this string is for a
	// log, and the member-facing figures travel as {minor, currency} in the
	// problem document instead.
	for _, want := range []string{"2500 EUR", "1500 EUR", "1000 EUR"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Error() = %q, want it to name %s", err.Error(), want)
		}
	}
}

// TestAWithdrawalStateIsSpeltAsTheSchemaSpellsIt. The strings are matched
// against withdrawal_request_state_known, so a second spelling anywhere is a
// state the database refuses to store.
func TestAWithdrawalStateIsSpeltAsTheSchemaSpellsIt(t *testing.T) {
	for state, want := range map[payout.State]string{
		payout.StateAwaitingApproval: "awaiting_approval",
		payout.StateApproved:         "approved",
		payout.StateRejected:         "rejected",
		payout.StatePaid:             "paid",
		payout.StateFailed:           "failed",
	} {
		if state.String() != want {
			t.Errorf("%v.String() = %q, want %q", state, state.String(), want)
		}
	}
}

// noDatabase satisfies [payout.Beginner] and is never reached: every case
// above is refused before a transaction is begun. A method that IS called
// fails the case rather than returning a plausible zero value.
type noDatabase struct{}

func (noDatabase) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	panic("payout: a refused request must not reach the database")
}

func (noDatabase) Query(context.Context, string, ...any) (pgx.Rows, error) {
	panic("payout: a refused request must not reach the database")
}

func (noDatabase) QueryRow(context.Context, string, ...any) pgx.Row {
	panic("payout: a refused request must not reach the database")
}

func (noDatabase) Begin(context.Context) (pgx.Tx, error) {
	panic("payout: a refused request must not begin a transaction")
}
