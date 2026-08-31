package wallet_test

// The contract for GET /wallet (T078): every status the document declares is
// one the endpoint actually produces, and every status it produces is one the
// document declares.
//
// The suite beside this one asserts what each answer MEANS. This one asserts
// the SET, which is the thing no behaviour test can: a status added to the
// handler and not to the document is a client that cannot handle it, and a
// status removed from the handler and left in the document is a client
// handling something that will never arrive. Both drift silently, because
// every individual test still passes.

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"slices"
	"sort"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// documentedStatuses reads what api/openapi.json promises for the given
// operation. Read from the file rather than restated here, because a copy is
// the drift this test exists to catch.
func documentedStatuses(t *testing.T, operationID string) []int {
	t.Helper()
	raw, err := os.ReadFile("../../../api/openapi.json")
	if err != nil {
		t.Fatalf("reading the contract: %v", err)
	}
	// A path item holds methods AND non-method keys - `parameters` is an
	// array - so each entry is decoded on its own and anything that is not
	// an operation is skipped rather than failing the parse.
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing the contract: %v", err)
	}
	for _, methods := range doc.Paths {
		for _, entry := range methods {
			var op struct {
				OperationID string                     `json:"operationId"`
				Responses   map[string]json.RawMessage `json:"responses"`
			}
			if err := json.Unmarshal(entry, &op); err != nil || op.OperationID != operationID {
				continue
			}
			codes := make([]int, 0, len(op.Responses))
			for code := range op.Responses {
				n, err := strconv.Atoi(code)
				if err != nil {
					t.Fatalf("the contract declares a non-numeric response %q for %s", code, operationID)
				}
				codes = append(codes, n)
			}
			sort.Ints(codes)
			return codes
		}
	}
	t.Fatalf("the contract describes no operation %q", operationID)
	return nil
}

// TestGetWalletServesEveryDocumentedStatusAndNoOther drives the endpoint to
// each documented status and compares the two sets in both directions.
func TestGetWalletServesEveryDocumentedStatusAndNoOther(t *testing.T) {
	t.Parallel()

	eur := money.Amount{Minor: 2000, Currency: "EUR"}
	cases := []struct {
		name   string
		status int
		build  func(t *testing.T) (*wallet.Wallets, wallet.MemberAuthenticator, string)
	}{
		{
			name:   "a member reads their own wallet",
			status: http.StatusOK,
			build: func(t *testing.T) (*wallet.Wallets, wallet.MemberAuthenticator, string) {
				return aWallet(t, eur), fakeAuth{token: "t", member: fakeMember}, "t"
			},
		},
		{
			name:   "a token belonging to nobody",
			status: http.StatusUnauthorized,
			build: func(t *testing.T) (*wallet.Wallets, wallet.MemberAuthenticator, string) {
				return aWallet(t, eur), fakeAuth{token: "t", member: fakeMember}, "wrong"
			},
		},
		{
			// A failure to reach the identity store is not a verdict on the
			// token, so it cannot be a 401.
			name:   "the authenticator could not be reached",
			status: http.StatusInternalServerError,
			build: func(t *testing.T) (*wallet.Wallets, wallet.MemberAuthenticator, string) {
				return aWallet(t, eur), fakeAuth{err: errors.New("connection reset")}, "t"
			},
		},
		{
			name:   "the deployment configured no threshold",
			status: http.StatusServiceUnavailable,
			build: func(t *testing.T) (*wallet.Wallets, wallet.MemberAuthenticator, string) {
				return aWallet(t, money.Amount{}), fakeAuth{token: "t", member: uuid.New()}, "t"
			},
		},
	}

	reached := make([]int, 0, len(cases))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, auth, token := tc.build(t)
			rec := serve(t, service, auth, walletRequest(t, token))
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.status, rec.Body)
			}
		})
		reached = append(reached, tc.status)
	}

	documented := documentedStatuses(t, "getWallet")
	sort.Ints(reached)
	reached = slices.Compact(reached)

	for _, status := range documented {
		if !slices.Contains(reached, status) {
			t.Errorf("the contract declares %d for getWallet and no case here produces it: a client is handling an answer that will never arrive", status)
		}
	}
	for _, status := range reached {
		if !slices.Contains(documented, status) {
			t.Errorf("the endpoint answers %d and the contract does not declare it: a client cannot handle it", status)
		}
	}
}
