package brand_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/platform/brand"
)

// fixtureDir holds the fixture brand ADR-0004 requires: a complete,
// valid brand that is deliberately unlike the one this repository
// currently ships — different name, different palette, different
// currency, different default language and place. Every test here works
// against it rather than against the live brand, so a test that passes
// is evidence the code reads the configuration rather than remembering
// the product it was written for.
const fixtureDir = "testdata/fixture"

// fixture is what testdata/fixture/brand.json means, written out
// longhand. The duplication is the assertion: if the file and this value
// ever disagree, one of them changed without anyone deciding to.
func fixture() brand.Brand {
	return brand.Brand{
		ID:   "zephyra",
		Name: "Zephyra",
		Legal: brand.Legal{
			Entity:       "Zephyra Fixture Kooperativ AB",
			Jurisdiction: "SE",
			Address:      "Kungsgatan 1, 411 19 Göteborg, Sweden",
			Documents: map[string]brand.Document{
				"terms":   {ID: "zephyra-terms", Version: "3.1.0"},
				"privacy": {ID: "zephyra-privacy", Version: "2026-05-04"},
				"cookies": {ID: "zephyra-cookies", Version: "1.0.0"},
			},
		},
		Domains: brand.Domains{
			Primary: "zephyra.example",
			Aliases: []string{"www.zephyra.example", "zephyra-forna.example"},
		},
		Support: brand.Support{
			General: "hej@zephyra.example",
			Legal:   "juridik@zephyra.example",
			Privacy: "dataskydd@www.zephyra.example",
		},
		Assets: brand.Assets{
			Logo:     "/brand/zephyra/logo.svg",
			LogoDark: "/brand/zephyra/logo-dark.svg",
			Favicon:  "/brand/zephyra/favicon.ico",
		},
		Theme: brand.Theme{
			Colours: map[string]string{
				"bg":       "#0b1d26",
				"surface":  "#123243",
				"text":     "#eef6f8",
				"accent":   "#2ec4b6",
				"accent-2": "#ffbf47",
			},
			Typography: brand.Typography{
				Heading:       `"Fraunces", Georgia, serif`,
				Body:          `"Public Sans", system-ui, sans-serif`,
				HeadingWeight: 700,
			},
		},
		Defaults: brand.Defaults{Language: "sv", Place: "goteborg", Currency: "SEK"},
		Payout:   brand.Payout{Descriptor: "ZEPHYRA CASHBACK"},
		Features: map[string]map[string]bool{
			"cashback": {"wallet": true, "withdrawal": true},
			"news":     {"reader": false},
		},
	}
}

func TestLoadDirReadsTheFixtureBrand(t *testing.T) {
	t.Parallel()

	got, err := brand.LoadDir(fixtureDir)
	if err != nil {
		t.Fatalf("LoadDir(%q): %v", fixtureDir, err)
	}
	if want := fixture(); !reflect.DeepEqual(got, want) {
		t.Errorf("LoadDir(%q)\n got %+v\nwant %+v", fixtureDir, got, want)
	}
}

func TestLoadReadsAnyFileSystem(t *testing.T) {
	t.Parallel()

	got, err := brand.Load(os.DirFS(fixtureDir), brand.FileName)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := fixture(); !reflect.DeepEqual(got, want) {
		t.Errorf("Load\n got %+v\nwant %+v", got, want)
	}
}

func TestLoadReportsAMissingFile(t *testing.T) {
	t.Parallel()

	_, err := brand.LoadDir(filepath.Join("testdata", "no-such-brand"))
	if err == nil {
		t.Fatal("LoadDir on a missing directory returned no error")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error %v does not wrap os.ErrNotExist", err)
	}
}

func TestParseRejectsMalformedDefinitions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data string
		want string
	}{
		{
			name: "not json at all",
			data: "not json",
			want: "brand: decode",
		},
		{
			// A key nobody reads is a value that silently does not
			// apply, which is how a surface keeps the previous
			// brand's colour and nobody can say why.
			name: "unknown field",
			data: `{"id":"x","tagline":"unknown to the schema"}`,
			want: "unknown field",
		},
		{
			name: "wrong type for a field",
			data: `{"id":42}`,
			want: "brand: decode",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			_, err := brand.Parse([]byte(testCase.data))
			if err == nil {
				t.Fatal("Parse accepted a malformed definition")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Errorf("error %q does not mention %q", err, testCase.want)
			}
		})
	}
}

func TestParseRejectsAWellFormedFileThatBreaksTheRules(t *testing.T) {
	t.Parallel()

	// Valid JSON, valid field names, and a brand no surface could be
	// rendered from. Decoding is not acceptance.
	_, err := brand.Parse([]byte(`{"id":"zephyra","name":"Zephyra"}`))
	if err == nil {
		t.Fatal("Parse accepted a brand with nothing but a name")
	}
	if !errors.Is(err, brand.ErrInvalid) {
		t.Errorf("error %v does not wrap ErrInvalid", err)
	}
}

func TestParseAcceptsTheFixtureFile(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join(fixtureDir, brand.FileName))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if _, err := brand.Parse(data); err != nil {
		t.Fatalf("Parse rejected the fixture brand: %v", err)
	}
}

func TestValidateAcceptsTheFixtureBrand(t *testing.T) {
	t.Parallel()

	if err := fixture().Validate(); err != nil {
		t.Fatalf("the fixture brand is invalid: %v", err)
	}
}

func TestValidateNamesEveryBrokenRule(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*brand.Brand)
		want   string
	}{
		{
			name:   "id is not a slug",
			mutate: func(b *brand.Brand) { b.ID = "Zephyra Brand" },
			want:   "id \"Zephyra Brand\" is not a slug",
		},
		{
			name:   "name is empty",
			mutate: func(b *brand.Brand) { b.Name = "  " },
			want:   "name is empty",
		},
		{
			name:   "name is padded",
			mutate: func(b *brand.Brand) { b.Name = "Zephyra " },
			want:   "leading or trailing whitespace",
		},
		{
			name:   "legal entity is empty",
			mutate: func(b *brand.Brand) { b.Legal.Entity = "" },
			want:   "legal.entity is empty",
		},
		{
			name:   "jurisdiction is not a country code",
			mutate: func(b *brand.Brand) { b.Legal.Jurisdiction = "sweden" },
			want:   "legal.jurisdiction \"sweden\"",
		},
		{
			name:   "legal address is empty",
			mutate: func(b *brand.Brand) { b.Legal.Address = "" },
			want:   "legal.address is empty",
		},
		{
			name:   "terms are missing",
			mutate: func(b *brand.Brand) { delete(b.Legal.Documents, brand.DocumentTerms) },
			want:   "legal.documents is missing \"terms\"",
		},
		{
			name:   "privacy notice is missing",
			mutate: func(b *brand.Brand) { delete(b.Legal.Documents, brand.DocumentPrivacy) },
			want:   "legal.documents is missing \"privacy\"",
		},
		{
			name: "document has no identifier",
			mutate: func(b *brand.Brand) {
				b.Legal.Documents[brand.DocumentTerms] = brand.Document{Version: "1.0.0"}
			},
			want: "legal.documents[\"terms\"].id is empty",
		},
		{
			name: "document has no version",
			mutate: func(b *brand.Brand) {
				b.Legal.Documents[brand.DocumentTerms] = brand.Document{ID: "zephyra-terms"}
			},
			want: "legal.documents[\"terms\"].version is empty",
		},
		{
			name:   "primary domain carries a scheme",
			mutate: func(b *brand.Brand) { b.Domains.Primary = "https://zephyra.example" },
			want:   "domains.primary",
		},
		{
			name:   "alias is not a host",
			mutate: func(b *brand.Brand) { b.Domains.Aliases = []string{"Zephyra.Example/path"} },
			want:   "domains.aliases[0]",
		},
		{
			name: "alias repeats the primary domain",
			mutate: func(b *brand.Brand) {
				b.Domains.Aliases = []string{"zephyra.example"}
			},
			want: "is a duplicate",
		},
		{
			name:   "support address is not an address",
			mutate: func(b *brand.Brand) { b.Support.General = "hej at zephyra example" },
			want:   "support.general",
		},
		{
			// The rebrand that looks finished and is not: the name
			// changed, the mailbox did not.
			name:   "support address is left on another brand's domain",
			mutate: func(b *brand.Brand) { b.Support.Legal = "juridik@someone-else.example" },
			want:   "is not on one of the brand's own domains",
		},
		{
			name:   "asset path is an absolute url",
			mutate: func(b *brand.Brand) { b.Assets.Logo = "https://cdn.example/logo.svg" },
			want:   "assets.logo",
		},
		{
			name:   "asset path escapes the web root",
			mutate: func(b *brand.Brand) { b.Assets.Favicon = "/brand/../../etc/favicon.ico" },
			want:   "escapes the web root",
		},
		{
			name:   "a required colour token is missing",
			mutate: func(b *brand.Brand) { delete(b.Theme.Colours, brand.ColourAccent) },
			want:   "theme.colours is missing \"accent\"",
		},
		{
			name:   "colour token name is not a slug",
			mutate: func(b *brand.Brand) { b.Theme.Colours["Accent Two"] = "#ffbf47" },
			want:   "theme.colours key \"Accent Two\"",
		},
		{
			name:   "colour is not lower-case six-digit hex",
			mutate: func(b *brand.Brand) { b.Theme.Colours[brand.ColourText] = "#EEF" },
			want:   "is not a lower-case six-digit hex colour",
		},
		{
			name:   "heading font stack is empty",
			mutate: func(b *brand.Brand) { b.Theme.Typography.Heading = "" },
			want:   "theme.typography.heading is empty",
		},
		{
			name:   "body font stack is empty",
			mutate: func(b *brand.Brand) { b.Theme.Typography.Body = "" },
			want:   "theme.typography.body is empty",
		},
		{
			name:   "heading weight is outside the css range",
			mutate: func(b *brand.Brand) { b.Theme.Typography.HeadingWeight = 0 },
			want:   "headingWeight 0",
		},
		{
			// A region subtag keys no catalogue: the catalogues are
			// keyed by the primary subtag alone (constitution VII).
			name:   "default language carries a region",
			mutate: func(b *brand.Brand) { b.Defaults.Language = "sv-SE" },
			want:   "defaults.language \"sv-SE\"",
		},
		{
			name:   "default place is not a slug",
			mutate: func(b *brand.Brand) { b.Defaults.Place = "Göteborg" },
			want:   "defaults.place",
		},
		{
			name:   "default currency is not iso 4217",
			mutate: func(b *brand.Brand) { b.Defaults.Currency = "kr" },
			want:   "defaults.currency \"kr\"",
		},
		{
			name:   "payout descriptor is empty",
			mutate: func(b *brand.Brand) { b.Payout.Descriptor = "" },
			want:   "payout.descriptor is empty",
		},
		{
			name:   "payout descriptor would be truncated",
			mutate: func(b *brand.Brand) { b.Payout.Descriptor = "ZEPHYRA CASHBACK REWARDS" },
			want:   "longer than 22 characters",
		},
		{
			name:   "payout descriptor is padded",
			mutate: func(b *brand.Brand) { b.Payout.Descriptor = " ZEPHYRA" },
			want:   "leading or trailing whitespace",
		},
		{
			name:   "payout descriptor is not ascii",
			mutate: func(b *brand.Brand) { b.Payout.Descriptor = "ZEPHYRA ÅTERBÄRING" },
			want:   "card schemes will transliterate",
		},
		{
			name:   "no product declares any flags",
			mutate: func(b *brand.Brand) { b.Features = nil },
			want:   "features declares no product",
		},
		{
			name:   "product key is not a slug",
			mutate: func(b *brand.Brand) { b.Features["Cash Back"] = map[string]bool{"wallet": true} },
			want:   "features key \"Cash Back\"",
		},
		{
			name:   "flag key is not a slug",
			mutate: func(b *brand.Brand) { b.Features["cashback"]["Instant Payout"] = true },
			want:   "features[\"cashback\"] key \"Instant Payout\"",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			b := fixture()
			testCase.mutate(&b)

			err := b.Validate()
			if err == nil {
				t.Fatal("Validate accepted a brand it should have rejected")
			}
			if !errors.Is(err, brand.ErrInvalid) {
				t.Errorf("error %v does not wrap ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Errorf("error %q does not mention %q", err, testCase.want)
			}
		})
	}
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	t.Parallel()

	// An empty brand breaks nearly every rule. Reporting them one per
	// run would make filling in a brand file an afternoon of guessing.
	err := brand.Brand{}.Validate()
	if err == nil {
		t.Fatal("the zero brand validated")
	}
	for _, want := range []string{"id \"\"", "name is empty", "legal.entity is empty", "payout.descriptor is empty"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestFeatureIsOffUnlessDeclaredOn(t *testing.T) {
	t.Parallel()

	b := fixture()
	tests := []struct {
		product string
		flag    string
		want    bool
	}{
		{product: "cashback", flag: "wallet", want: true},
		{product: "cashback", flag: "withdrawal", want: true},
		{product: "news", flag: "reader", want: false},
		{product: "cashback", flag: "instant-payout", want: false},
		{product: "grocery", flag: "wallet", want: false},
	}

	for _, testCase := range tests {
		t.Run(testCase.product+"/"+testCase.flag, func(t *testing.T) {
			t.Parallel()

			if got := b.Feature(testCase.product, testCase.flag); got != testCase.want {
				t.Errorf("Feature(%q, %q) = %t, want %t", testCase.product, testCase.flag, got, testCase.want)
			}
		})
	}
}

func TestDocumentReportsWhetherTheBrandHasOne(t *testing.T) {
	t.Parallel()

	b := fixture()

	doc, ok := b.Document(brand.DocumentTerms)
	if !ok {
		t.Fatal("the fixture brand has no terms document")
	}
	if doc.Version != "3.1.0" {
		t.Errorf("terms version = %q, want %q", doc.Version, "3.1.0")
	}

	if _, ok := b.Document("shareholder-agreement"); ok {
		t.Error("Document reported a kind the brand does not publish")
	}
}

func TestHostsAndAddressesAndPathsKeepTheirOrder(t *testing.T) {
	t.Parallel()

	b := fixture()

	wantHosts := []string{"zephyra.example", "www.zephyra.example", "zephyra-forna.example"}
	if got := b.Domains.Hosts(); !reflect.DeepEqual(got, wantHosts) {
		t.Errorf("Hosts() = %v, want %v", got, wantHosts)
	}

	wantAddresses := []string{"hej@zephyra.example", "juridik@zephyra.example", "dataskydd@www.zephyra.example"}
	if got := b.Support.Addresses(); !reflect.DeepEqual(got, wantAddresses) {
		t.Errorf("Addresses() = %v, want %v", got, wantAddresses)
	}

	wantPaths := []string{"/brand/zephyra/logo.svg", "/brand/zephyra/logo-dark.svg", "/brand/zephyra/favicon.ico"}
	if got := b.Assets.Paths(); !reflect.DeepEqual(got, wantPaths) {
		t.Errorf("Paths() = %v, want %v", got, wantPaths)
	}
}
