// Package ledgersdk_test is the executable half of the Blnk Go SDK pin.
//
// go.mod records the version; this package records that the version is
// USABLE - that it compiles against this module's Go version, that a client
// can be built from it, and that the surface the cashback ledger port needs
// (ledgers, balances, transactions) exists on that client. A pin nothing
// imports is a line in a file that `go mod tidy` deletes; a pin with a test
// against it is a decision the build keeps.
//
// It is deliberately test-only: no non-test Go file lives here, so this
// directory contributes no statements to the repository's coverage gate and
// nothing here is linked into the binary. The SDK reaches the binary through
// internal/cashback/wallet/blnk (T043) and nowhere else - ADR-0002 requires
// that no Blnk type crosses that package's boundary.
//
// The rationale for the version chosen is in docs/RELEASING.md.
package ledgersdk_test

import (
	"net/url"
	"os"
	"testing"

	blnkgo "github.com/blnkfinance/blnk-go"
)

// pinnedBaseURL is any syntactically valid endpoint: the offline half of
// this test builds a client and never calls it.
const pinnedBaseURL = "http://ledger.invalid:5001"

// TestPinnedSDKBuildsAClient is the compile-and-construct proof. It needs no
// network, no Docker and no ledger, so it runs on every machine and in every
// job - which is the point: the pin is checked wherever the code is built,
// not only where the stack happens to be up.
func TestPinnedSDKBuildsAClient(t *testing.T) {
	t.Parallel()

	base, err := url.Parse(pinnedBaseURL)
	if err != nil {
		t.Fatalf("parsing the base URL: %v", err)
	}
	key := "not-a-real-key"
	client := blnkgo.NewClient(base, &key)
	if client == nil {
		t.Fatal("NewClient returned nil")
	}

	// The three services the Ledger port is built on (contracts/ports.md
	// §1): a ledger to hold accounts, balances to be the accounts, and
	// transactions to move between them. If a future version renames or
	// removes one of these, this fails at compile time on the bump rather
	// than at runtime on the deploy.
	if client.Ledger == nil {
		t.Error("client.Ledger is nil")
	}
	if client.LedgerBalance == nil {
		t.Error("client.LedgerBalance is nil")
	}
	if client.Transaction == nil {
		t.Error("client.Transaction is nil")
	}
}

// TestPinnedSDKCarriesTheIdempotencyKey pins the field the whole exactly-once
// story rests on (C-5). Blnk's idempotency key is the transaction's
// Reference, and a version that renamed it would break payout replay
// protection silently. Constructing the request is enough to prove the field
// exists and is a string; nothing is sent.
func TestPinnedSDKCarriesTheIdempotencyKey(t *testing.T) {
	t.Parallel()

	const key = "apivo-withdrawal-0000-0000"
	req := blnkgo.CreateTransactionRequest{
		ParentTransaction: blnkgo.ParentTransaction{
			Reference: key,
			Currency:  "EUR",
			Precision: 100,
		},
	}
	if req.Reference != key {
		t.Fatalf("Reference = %q, want %q", req.Reference, key)
	}
}

// TestPinnedSDKReachesARunningLedger is the other half - usable, not merely
// importable - and it can only run where a ledger exists. It is keyed on
// BLNK_URL exactly as the invariant suites are keyed on DATABASE_URL:
// skipped on the founder's machine while Docker Desktop is unavailable, and
// never skipped in the cashback CI job, which is the verification of record.
func TestPinnedSDKReachesARunningLedger(t *testing.T) {
	t.Parallel()

	raw := os.Getenv("BLNK_URL")
	if raw == "" {
		t.Skip("BLNK_URL is unset: no ledger to reach (expected without Docker)")
	}
	base, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("BLNK_URL is not a URL: %v", err)
	}
	key := os.Getenv("BLNK_SECRET_KEY")
	client := blnkgo.NewClient(base, &key)

	// Listing ledgers is the cheapest real round-trip: it proves the
	// transport, the base-URL handling and the response decoding, and it
	// creates nothing. An empty list is a pass - the assertion is that the
	// call succeeded, not that anything has been posted yet.
	if _, _, err := client.Ledger.List(); err != nil {
		t.Fatalf("listing ledgers through the pinned SDK: %v", err)
	}
}
