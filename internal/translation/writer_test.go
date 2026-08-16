package translation

// Unit tests for the classification that decides whether a failed insert
// books its spend. The stakes are asymmetric and the tests spell them out:
// booking a transient failure double-counts the month when the caller's
// retry succeeds (the trigger books the same spend again), while booking
// nothing on a deterministic refusal loses real money from the ledger.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestDeterministicRefusalsBookSpendAndTransientErrorsDoNot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		// The refusals: the same statement fails the same way every time,
		// so the call's result will never be stored and its cost must be
		// booked now.
		{"unique violation", &pgconn.PgError{Code: "23505"}, true},
		{"foreign key violation (unknown locale)", &pgconn.PgError{Code: "23503"}, true},
		{"check violation", &pgconn.PgError{Code: "23514"}, true},
		{"not null violation", &pgconn.PgError{Code: "23502"}, true},
		{"data exception (malformed value)", &pgconn.PgError{Code: "22001"}, true},
		{"invalid statement", &pgconn.PgError{Code: "42703"}, true},
		{"wrapped refusal still classifies", fmt.Errorf("inserting: %w", &pgconn.PgError{Code: "23505"}), true},

		// The transients: a retry may succeed, and the trigger would then
		// book the same spend a second time on top of a booking made here.
		{"deadlock victim", &pgconn.PgError{Code: "40P01"}, false},
		{"serialization failure", &pgconn.PgError{Code: "40001"}, false},
		{"statement_timeout", &pgconn.PgError{Code: "57014"}, false},
		{"admin shutdown", &pgconn.PgError{Code: "57P01"}, false},
		{"connection failure", &pgconn.PgError{Code: "08006"}, false},
		{"no server verdict at all", errors.New("read tcp: connection reset by peer"), false},
		{"nil-adjacent plain error", errors.New("context deadline exceeded"), false},

		// The default direction is deliberate: an unrecognised class reads
		// as transient, because misreading transient as deterministic
		// corrupts the ledger silently while the reverse keeps returning
		// a loud error until someone looks.
		{"unrecognised class", &pgconn.PgError{Code: "XX000"}, false},
		{"empty code", &pgconn.PgError{Code: ""}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isDeterministicRefusal(tt.err); got != tt.want {
				t.Errorf("isDeterministicRefusal(%v) = %t, want %t", tt.err, got, tt.want)
			}
		})
	}
}

// TestRecordFailedCallRefusesUnconfiguredCaps mirrors Record: a missing
// budget is not an unlimited one, and validating before any statement runs
// means a nil database is never touched - which is also what lets this
// test run without one.
func TestRecordFailedCallRefusesUnconfiguredCaps(t *testing.T) {
	t.Parallel()

	writer := NewWriter(nil, Caps{})
	_, err := writer.RecordFailedCall(context.Background(), Spend{CostMicroUSD: 1})
	if !errors.Is(err, ErrCapsNotConfigured) {
		t.Fatalf("RecordFailedCall() error = %v, want ErrCapsNotConfigured", err)
	}
}
