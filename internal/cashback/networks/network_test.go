// The tests for network.go: the smallest adapter that satisfies the port, the
// wiring-time check held against it, and the two properties the iterator
// shape was chosen for. It also guards the sentinel taxonomy and the port
// declaration's own imports, both of which are statements about the package
// as a whole.

package networks_test

import (
	"context"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"iter"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// portTestAccountID is the cashback.network_account row every adapter below
// polls on behalf of.
var portTestAccountID = uuid.MustParse("2f1c0e9a-6b3d-4f7a-9c21-5d8e4a0b7c63")

// portTestMustAccount builds a publisher account or stops the run. It is used
// only for accounts the tests assert are well formed, so a failure here is a
// broken fixture rather than a finding.
func portTestMustAccount(id uuid.UUID, network networks.NetworkID, externalID string) networks.PublisherAccount {
	account, err := networks.NewPublisherAccount(id, network, externalID)
	if err != nil {
		panic("networks_test: fixture publisher account: " + err.Error())
	}
	return account
}

// portTestAccount is that account as the port carries it: the publisher
// account whose cursors an adapter's poll advances.
var portTestAccount = portTestMustAccount(portTestAccountID, portTestNetworkID, "awin-publisher-77021")

// TestPublisherAccountNamesARow pins what the poller needs from the port that
// nothing else can supply: network_transaction.network_account_id is NOT NULL
// and both durable cursors live on the account, so an account that is not a
// real row is an adapter whose evidence cannot be written and whose cursor
// never moves.
func TestPublisherAccountNamesARow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		id         uuid.UUID
		network    networks.NetworkID
		externalID string
		wantErr    error
	}{
		{name: "an Awin publisher account", id: portTestAccountID, network: portTestNetworkID, externalID: "awin-publisher-77021"},
		{name: "no row id, which a bare struct literal carries", network: portTestNetworkID, externalID: "awin-publisher-77021", wantErr: networks.ErrInvalidPublisherAccount},
		{name: "an account at a network nobody can name", id: portTestAccountID, network: "Awin", externalID: "awin-publisher-77021", wantErr: networks.ErrInvalidPublisherAccount},
		{name: "an account with no publisher id at the network", id: portTestAccountID, network: portTestNetworkID, externalID: " ", wantErr: networks.ErrInvalidPublisherAccount},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			account, err := networks.NewPublisherAccount(tc.id, tc.network, tc.externalID)
			portTestAssert(t, "NewPublisherAccount()", err, tc.wantErr, nil)
			if tc.wantErr != nil {
				if account != (networks.PublisherAccount{}) {
					t.Errorf("NewPublisherAccount() returned %s beside its refusal", account)
				}
				return
			}
			if account.ID() != tc.id || account.Network() != tc.network || account.ExternalID() != tc.externalID {
				t.Errorf("NewPublisherAccount() = %s, want the three facts back unchanged", account)
			}
		})
	}

	t.Run("two accounts at one network are two accounts", func(t *testing.T) {
		t.Parallel()

		second := portTestMustAccount(uuid.MustParse("9d4b1c77-2a5e-4c19-8f03-6b1e2d7a45c8"), portTestNetworkID, "awin-publisher-88134")
		if second.ID() == portTestAccount.ID() {
			t.Fatalf("the fixture accounts share a row id, so this test proves nothing")
		}
		if second == portTestAccount {
			t.Errorf("two publisher accounts at one network compared equal; the poller keys its registry on this")
		}
		if second.String() == portTestAccount.String() {
			t.Errorf("two publisher accounts at one network both render as %q", second.String())
		}
	})
}

// TestNetworkSentinelsAreDistinct guards the taxonomy itself: the
// conformance suite tells failures apart with errors.Is, so two sentinels
// that aliased each other would let an adapter pass the wrong test - and the
// retry classification would answer the wrong question about a frozen cursor.
func TestNetworkSentinelsAreDistinct(t *testing.T) {
	t.Parallel()

	sentinels := []error{
		networks.ErrInvalidNetworkID,
		networks.ErrInvalidPublisherAccount,
		networks.ErrMissingExternalID,
		networks.ErrBlankClickRef,
		networks.ErrMalformedClickRef,
		networks.ErrInvalidIssuedClickRef,
		networks.ErrMissingStatusRaw,
		networks.ErrUnmappableStatus,
		networks.ErrMissingRawPayload,
		networks.ErrMalformedRawPayload,
		networks.ErrMissingTransactedAt,
		networks.ErrMissingMerchantName,
		networks.ErrInvalidMerchantCountry,
		networks.ErrInvalidQueryWindow,
		networks.ErrWindowTooWide,
		networks.ErrInvalidLimits,
		networks.ErrDeeplinkNotFormed,
		networks.ErrDeeplinkInputsRefused,
		networks.ErrIterationAbandoned,
		networks.ErrNetworkUnavailable,
		networks.ErrNetworkRefused,
		networks.ErrNetworkRateLimited,
	}
	for i, a := range sentinels {
		for j, b := range sentinels {
			if got, want := errors.Is(a, b), i == j; got != want {
				t.Errorf("errors.Is(%v, %v) = %v, want %v", a, b, got, want)
			}
		}
	}
}

// portTestAdapter is the smallest thing that satisfies the port: it proves
// the interface is implementable as declared, and gives the iteration tests
// below something to range over. It is not a fixture adapter - that is T050,
// in its own package, with recorded payloads - but it is the shape an adapter
// author copies, so it honours the context and contract rule 8 rather than
// returning quietly.
type portTestAdapter struct {
	// id, account and limits are what the adapter says about itself, held to
	// the port by networks.ValidateNetwork.
	id      networks.NetworkID
	account networks.PublisherAccount
	limits  networks.Limits
	// reports is what FetchTransactions yields, in order.
	reports []networks.Reported
	// failAfter is how many reports are yielded before a streaming failure
	// is reported instead; a negative value means none.
	failAfter int
	// fetched counts how many reports the iterator actually produced, so a
	// test can prove that stopping early stops the work.
	fetched int
}

// portTestNewAdapter builds a well-formed adapter; the cases below break one
// of its declarations at a time.
func portTestNewAdapter(reports ...networks.Reported) *portTestAdapter {
	return &portTestAdapter{
		id:        portTestNetworkID,
		account:   portTestAccount,
		limits:    portTestLimits,
		reports:   reports,
		failAfter: -1,
	}
}

func (a *portTestAdapter) ID() networks.NetworkID { return a.id }

func (a *portTestAdapter) Account() networks.PublisherAccount { return a.account }

func (a *portTestAdapter) Limits() networks.Limits { return a.limits }

func (a *portTestAdapter) BuildDeeplink(_ context.Context, target networks.DeeplinkTarget, ref networks.IssuedClickRef) (string, error) {
	if err := networks.ValidateDeeplinkInputs(a.ID(), target, ref); err != nil {
		return "", err
	}
	return target.Template + "&" + target.ClickRefParam + "=" + ref.Ref(), nil
}

func (a *portTestAdapter) FetchTransactions(ctx context.Context, window networks.QueryWindow) (iter.Seq2[networks.Reported, error], error) {
	if err := a.Limits().ValidateWindow(window); err != nil {
		return nil, err
	}
	return func(yield func(networks.Reported, error) bool) {
		for i, report := range a.reports {
			// Contract rule 8. Returning here instead would hand the caller
			// a half-read window that looks exactly like a whole one.
			if err := ctx.Err(); err != nil {
				yield(networks.Reported{}, networks.AbandonedIteration(err))
				return
			}
			if a.failAfter >= 0 && i == a.failAfter {
				yield(networks.Reported{}, fmt.Errorf("%w: the network stopped answering", networks.ErrNetworkUnavailable))
				return
			}
			a.fetched++
			if !yield(report, nil) {
				return
			}
		}
	}, nil
}

func (a *portTestAdapter) FetchCatalogue(_ context.Context) (iter.Seq2[networks.ReportedMerchant, error], error) {
	return func(yield func(networks.ReportedMerchant, error) bool) {
		yield(portTestMerchant(), nil)
	}, nil
}

// The compile-time proof that the port is satisfiable as declared - that the
// two iterator signatures are ones an adapter can actually return, and that
// no method needs a type an adapter package cannot name.
var _ networks.Network = (*portTestAdapter)(nil)

// portTestPortOf widens an adapter to the port, so the tests below exercise
// the interface rather than the concrete type behind it.
func portTestPortOf(a *portTestAdapter) networks.Network { return a }

// TestValidateNetworkHoldsAnAdapterToItsOwnDeclarations pins the check the
// composition root makes at wiring, which is the only place these are cheap.
// An adapter with a mistyped id fetches transactions perfectly well and fails
// at the foreign key of the first INSERT of a window already fully fetched;
// one whose account is at another network files one network's transactions
// under another's id; one with an unset Limits refuses every window it is
// given.
func TestValidateNetworkHoldsAnAdapterToItsOwnDeclarations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		adapter func() *portTestAdapter
		wantErr error
		wantIn  []string
	}{
		{
			name:    "a wired Awin adapter",
			adapter: func() *portTestAdapter { return portTestNewAdapter() },
		},
		{
			name: "an adapter whose id is not the word the network table is keyed by",
			adapter: func() *portTestAdapter {
				a := portTestNewAdapter()
				a.id = "Awin"
				return a
			},
			wantErr: networks.ErrInvalidNetworkID,
			wantIn:  []string{`"Awin"`},
		},
		{
			name: "an adapter wired with no publisher account",
			adapter: func() *portTestAdapter {
				a := portTestNewAdapter()
				a.account = networks.PublisherAccount{}
				return a
			},
			wantErr: networks.ErrInvalidPublisherAccount,
		},
		{
			name: "an adapter polling an account held at another network",
			adapter: func() *portTestAdapter {
				a := portTestNewAdapter()
				a.account = portTestMustAccount(portTestAccountID, "tradedoubler", "td-publisher-1")
				return a
			},
			wantErr: networks.ErrInvalidPublisherAccount,
			wantIn:  []string{`"tradedoubler"`},
		},
		{
			name: "an adapter built with a forgotten Limits",
			adapter: func() *portTestAdapter {
				a := portTestNewAdapter()
				a.limits = networks.Limits{}
				return a
			},
			wantErr: networks.ErrInvalidLimits,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := networks.ValidateNetwork(portTestPortOf(tc.adapter()))
			portTestAssert(t, "ValidateNetwork()", err, tc.wantErr, tc.wantIn)
		})
	}
}

// TestNetworkPortStopsEarlyAndSurfacesErrors pins the properties the iterator
// shape was chosen for: a caller that stops mid-window stops the adapter's
// work, a failure part-way through a window reaches that caller per item
// rather than being swallowed, and - the one that a durable cursor rests on -
// an adapter that gives up mid-window says so out loud. All three are what
// makes contract rule 4 safe: a caller that stopped must be able to run the
// window again from the beginning, having persisted only what it saw.
func TestNetworkPortStopsEarlyAndSurfacesErrors(t *testing.T) {
	t.Parallel()

	first, second := portTestReport(), portTestReport()
	second.ExternalID = "awin-tx-90211"

	t.Run("a caller that stops early stops the work", func(t *testing.T) {
		t.Parallel()

		adapter := portTestNewAdapter(first, second)
		seq, err := portTestPortOf(adapter).FetchTransactions(t.Context(), portTestWindow())
		if err != nil {
			t.Fatalf("FetchTransactions() = %v, want nil", err)
		}
		var seen int
		for report, err := range seq {
			if err != nil {
				t.Fatalf("iteration yielded %v, want nil", err)
			}
			if err := report.Validate(); err != nil {
				t.Errorf("a yielded report is invalid: %v", err)
			}
			seen++
			break
		}
		if seen != 1 || adapter.fetched != 1 {
			t.Errorf("saw %d report(s) and the adapter produced %d, want 1 and 1", seen, adapter.fetched)
		}
	})

	t.Run("a streaming failure reaches the caller classified", func(t *testing.T) {
		t.Parallel()

		adapter := portTestNewAdapter(first, second)
		adapter.failAfter = 1
		seq, err := portTestPortOf(adapter).FetchTransactions(t.Context(), portTestWindow())
		if err != nil {
			t.Fatalf("FetchTransactions() = %v, want nil", err)
		}
		var seen int
		var streamErr error
		for _, err := range seq {
			if err != nil {
				streamErr = err
				break
			}
			seen++
		}
		if seen != 1 {
			t.Errorf("saw %d report(s) before the failure, want 1", seen)
		}
		if !errors.Is(streamErr, networks.ErrNetworkUnavailable) {
			t.Errorf("iteration ended with %v, want a failure wrapping %v: rule 4 re-runs the whole window, so a caller must know whether re-running can ever succeed",
				streamErr, networks.ErrNetworkUnavailable)
		}
		if errors.Is(streamErr, networks.ErrNetworkRefused) {
			t.Errorf("a transient failure = %v, which a caller would abandon the account over", streamErr)
		}
	})

	t.Run("an adapter that gives up mid-window says so", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		adapter := portTestNewAdapter(first, second)
		seq, err := portTestPortOf(adapter).FetchTransactions(ctx, portTestWindow())
		if err != nil {
			t.Fatalf("FetchTransactions() = %v, want nil", err)
		}
		cancel()

		var seen int
		var streamErr error
		for _, err := range seq {
			if err != nil {
				streamErr = err
				break
			}
			seen++
		}
		if seen != 0 {
			t.Errorf("saw %d report(s) after cancellation, want 0", seen)
		}
		if !errors.Is(streamErr, networks.ErrIterationAbandoned) {
			t.Fatalf("a cancelled window ended with %v, want an error wrapping %v; a loop that ends silently is what a cursor advances on",
				streamErr, networks.ErrIterationAbandoned)
		}
		if !errors.Is(streamErr, context.Canceled) {
			t.Errorf("a cancelled window ended with %v, want it to carry why", streamErr)
		}
	})

	t.Run("a window wider than the network allows never reaches the network", func(t *testing.T) {
		t.Parallel()

		adapter := portTestNewAdapter(first, second)
		wide := networks.QueryWindow{From: portTestAnchor, To: portTestAnchor.Add(90 * 24 * time.Hour)}
		seq, err := portTestPortOf(adapter).FetchTransactions(t.Context(), wide)
		if !errors.Is(err, networks.ErrWindowTooWide) {
			t.Fatalf("FetchTransactions() = %v, want an error wrapping %v", err, networks.ErrWindowTooWide)
		}
		if seq != nil {
			t.Errorf("FetchTransactions() returned an iterator beside its refusal")
		}
		if adapter.fetched != 0 {
			t.Errorf("the adapter produced %d report(s) for a refused window, want 0", adapter.fetched)
		}
	})
}

// TestNetworkPortBuildsDeeplinkThroughTheInterface proves an adapter reached
// through the port can form a redirect carrying the click reference in the
// network's own parameter (FR-021), and refuses rather than returning a
// half-built one.
func TestNetworkPortBuildsDeeplinkThroughTheInterface(t *testing.T) {
	t.Parallel()

	adapter := portTestPortOf(portTestNewAdapter())
	redirect, err := adapter.BuildDeeplink(t.Context(), portTestTarget(), portTestIssuedRef)
	if err != nil {
		t.Fatalf("BuildDeeplink() = %v, want nil", err)
	}
	if !strings.Contains(redirect, "clickref="+portTestRefValue) {
		t.Errorf("BuildDeeplink() = %q, want the click reference in the network's own parameter", redirect)
	}
	if !strings.HasPrefix(redirect, "https://") {
		t.Errorf("BuildDeeplink() = %q, want an absolute https URL: it is written straight into a Location header", redirect)
	}

	redirect, err = adapter.BuildDeeplink(t.Context(), portTestTarget(), networks.IssuedClickRef{})
	if !errors.Is(err, networks.ErrDeeplinkNotFormed) {
		t.Fatalf("BuildDeeplink() with no click reference = %v, want an error wrapping %v",
			err, networks.ErrDeeplinkNotFormed)
	}
	if redirect != "" {
		t.Errorf("BuildDeeplink() = %q beside its refusal; a partially-formed URL still redirects", redirect)
	}
}

// TestPortDeclarationNamesNoVendor is the structural half of contract rule 6.
// Adapters must never write to the database, and the way this package helps
// is by handing them no means to: the port file speaks only its own types,
// platform/money and the one identifier type the repository shares, so a
// database driver and a generated store are never in an adapter's dependency
// graph by way of the port it satisfies.
//
// It reads the port's own file rather than the package, because the poller
// and the evidence writer that will live beside it necessarily know the
// database - it is the DECLARATION that must not. The repository-wide rule
// (SC-008) is T109's and is not written yet, which is exactly why this narrow
// one is worth having now.
func TestPortDeclarationNamesNoVendor(t *testing.T) {
	t.Parallel()

	const portFile = "network.go"
	allowed := map[string]bool{
		"github.com/google/uuid":                                  true,
		"github.com/Nomos-N4s/apivo-news/internal/platform/money": true,
	}

	src, err := os.ReadFile(portFile)
	if err != nil {
		t.Fatalf("reading %s: %v; if the port declaration moved, point this test at the file that holds it rather than deleting the rule", portFile, err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), portFile, src, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parsing %s: %v", portFile, err)
	}

	var judged int
	for _, imported := range file.Imports {
		path, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			t.Fatalf("unquoting import %s: %v", imported.Path.Value, err)
		}
		// The standard library is everything whose first path segment names
		// no host, and it is nobody's vendor problem.
		if !strings.Contains(path, ".") {
			continue
		}
		judged++
		if !allowed[path] {
			t.Errorf("%s imports %q; a port that names a vendor hands every adapter that vendor's types, and contract rule 6 says an adapter translates and nothing else. Take what the port needs as a value of its own.",
				portFile, path)
		}
	}
	if judged == 0 {
		t.Fatalf("%s imports nothing outside the standard library, so this rule judged nothing and passed vacuously; if the port genuinely needs no such import, delete the rule rather than leaving it green", portFile)
	}
}
