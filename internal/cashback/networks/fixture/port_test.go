// The tests for port.go, and the only ones here that see this package the way
// the composition root does - from outside it. [New] refuses what wiring
// refuses, and nothing in the package was handed the means to write a row.

package fixture_test

import (
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/fixture"
)

// fixtureTestPortAccount is a publisher account at this adapter's own network,
// built through the exported surface the composition root would use.
func fixtureTestPortAccount(t *testing.T) networks.PublisherAccount {
	t.Helper()
	account, err := networks.NewPublisherAccount(uuid.New(), fixture.ID, "fixture-publisher-1")
	if err != nil {
		t.Fatalf("building the publisher account: %v", err)
	}
	return account
}

// TestNewProducesAnAdapterWiringAccepts is the whole of what New adds over
// assembling the struct: the finished adapter is held to what the composition
// root would hold it to, so a mistake in an id, an account or a set of limits
// is found at wiring rather than at the foreign key of the first INSERT of a
// window that had already been fully fetched.
func TestNewProducesAnAdapterWiringAccepts(t *testing.T) {
	t.Parallel()

	adapter, err := fixture.New(fixtureTestPortAccount(t))
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	if err := networks.ValidateNetwork(adapter); err != nil {
		t.Fatalf("ValidateNetwork() refused an adapter New had already accepted: %v", err)
	}
}

func TestNewRefusesWhatWiringWouldRefuse(t *testing.T) {
	t.Parallel()

	otherNetwork, err := networks.NewPublisherAccount(uuid.New(), networks.NetworkID("awin"), "pub-9")
	if err != nil {
		t.Fatalf("building the other network's account: %v", err)
	}

	tests := []struct {
		name    string
		account networks.PublisherAccount
		opts    []fixture.Option
		wantErr error
	}{
		{
			name:    "an account nobody built",
			account: networks.PublisherAccount{},
			wantErr: networks.ErrInvalidPublisherAccount,
		},
		{
			name:    "an account held at another network",
			account: otherNetwork,
			wantErr: networks.ErrInvalidPublisherAccount,
		},
		{
			name:    "limits that describe no queryable network",
			account: fixtureTestPortAccount(t),
			opts:    []fixture.Option{fixture.WithLimits(networks.Limits{})},
			wantErr: networks.ErrInvalidLimits,
		},
		{
			name:    "a rate nobody may make a request under",
			account: fixtureTestPortAccount(t),
			opts:    []fixture.Option{fixture.WithLimits(networks.Limits{MaxWindow: time.Hour})},
			wantErr: networks.ErrInvalidLimits,
		},
		{
			name:    "a stage the recording has not",
			account: fixtureTestPortAccount(t),
			opts:    []fixture.Option{fixture.WithStage(fixture.Stage(99))},
			wantErr: fixture.ErrOptionRefused,
		},
		{
			name:    "an option that is nil",
			account: fixtureTestPortAccount(t),
			opts:    []fixture.Option{nil},
			wantErr: fixture.ErrOptionRefused,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			adapter, err := fixture.New(tc.account, tc.opts...)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("New() error = %v, want one wrapping %v", err, tc.wantErr)
			}
			if adapter != nil {
				t.Error("New() returned an adapter beside its refusal; a half-built one would poll on behalf of nobody")
			}
		})
	}
}

// TestFixtureNamesNoDatabase is the structural half of contract rule 6.
// Adapters translate and nothing else: they open no transaction, write no row
// and decide no credit. Nothing can hold an adapter to a rule about what it
// must not contain, so what this does instead is refuse the means - a driver,
// a generated store, a ledger SDK - anywhere in the package, tests included,
// because the first such import is likeliest to show up in a test that wanted
// to check a row.
//
// It reads every file in the package rather than one declaration, because
// unlike the port there is no single file here that speaks for the rule. The
// repository-wide rule that seals an adapter inside its own package (SC-008)
// is T109's and is not written yet, which is exactly why this narrow one is
// worth having now.
func TestFixtureNamesNoDatabase(t *testing.T) {
	t.Parallel()

	allowed := map[string]bool{
		"github.com/google/uuid":                                     true,
		"github.com/Nomos-N4s/apivo-news/internal/platform/money":    true,
		"github.com/Nomos-N4s/apivo-news/internal/cashback/networks": true,
		// This package's own external test package imports it, which is how
		// the tests in this file reach it at all.
		"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/fixture": true,
	}

	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("listing the package's files: %v", err)
	}
	if len(sources) == 0 {
		t.Fatal("no Go file was read, so this rule judged nothing and passed vacuously")
	}

	judged := 0
	for _, source := range sources {
		src, err := os.ReadFile(source)
		if err != nil {
			t.Fatalf("reading %s: %v", source, err)
		}
		file, err := parser.ParseFile(token.NewFileSet(), source, src, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", source, err)
		}
		for _, imported := range file.Imports {
			path, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				t.Fatalf("unquoting import %s in %s: %v", imported.Path.Value, source, err)
			}
			// The standard library is everything whose first path segment
			// names no host, and it is nobody's vendor problem.
			if !strings.Contains(path, ".") {
				continue
			}
			judged++
			if !allowed[path] {
				t.Errorf("%s imports %q; an adapter handed a driver or a store is an adapter that can write a row, and contract rule 6 says it translates and nothing else",
					source, path)
			}
		}
	}
	if judged == 0 {
		t.Fatal("the package imports nothing outside the standard library, so this rule judged nothing and passed vacuously; if that is genuinely true, delete the rule rather than leaving it green")
	}
}
