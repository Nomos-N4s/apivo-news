package main

// Whether a deployment gets a catalogue import, and what it is told when it
// does not (T105).
//
// The decision has three inputs from three places, and every wrong answer is
// a quiet one: an import built without a brand would stamp routes with an
// empty tenant, one built without a language would label every fallback
// wrongly, and one silently not scheduled would leave a catalogue that looks
// exactly like a working catalogue until a member clicks a retailer who left.

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/fixture"
	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
)

// anAdapter is a network to hand the wiring. Which network it is does not
// matter here - what is under test is whether a job is built at all.
func anAdapter(t *testing.T) networks.Network {
	t.Helper()
	account, err := networks.NewPublisherAccount(uuid.New(), networks.NetworkID("fixture"), "123456")
	if err != nil {
		t.Fatalf("NewPublisherAccount(): %v", err)
	}
	adapter, err := fixture.New(account)
	if err != nil {
		t.Fatalf("fixture.New(): %v", err)
	}
	return adapter
}

// configured is a deployment with everything the import needs.
func configured(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		BrandDir: brandDir(t),
		Cashback: config.CashbackConfig{
			Enabled: true,
			Network: config.NetworkConfig{Driver: config.NetworkDriverFixture, SourceLanguage: "en"},
		},
	}
}

func TestAFullyConfiguredDeploymentGetsACatalogueImport(t *testing.T) {
	t.Parallel()
	ctx, pool := opsWiringPool(t)
	_ = ctx

	imports, missing, err := newCatalogueImport(discardLogger(), configured(t), anAdapter(t), pool)
	if err != nil {
		t.Fatalf("newCatalogueImport(): %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("a fully configured deployment reports %v missing", missing)
	}
	if imports == nil {
		t.Fatal("a fully configured deployment got no catalogue import")
	}
}

// TestAnUnconfiguredDeploymentIsToldWhichKeyItLacks. Not a startup failure -
// a deployment configured ahead of an operator should serve everything else -
// but never a silent one, which is why the keys come back rather than a bare
// nil.
func TestAnUnconfiguredDeploymentIsToldWhichKeyItLacks(t *testing.T) {
	t.Parallel()
	ctx, pool := opsWiringPool(t)
	_ = ctx

	noLanguage := configured(t)
	noLanguage.Cashback.Network.SourceLanguage = ""
	noBrand := configured(t)
	noBrand.BrandDir = ""
	neither := config.Config{Cashback: config.CashbackConfig{Enabled: true}}

	for name, one := range map[string]struct {
		cfg  config.Config
		want []string
	}{
		"no source language": {noLanguage, []string{"NETWORK_SOURCE_LANGUAGE"}},
		"no brand":           {noBrand, []string{"BRAND_DIR"}},
		"neither":            {neither, []string{"NETWORK_SOURCE_LANGUAGE", "BRAND_DIR"}},
	} {
		imports, missing, err := newCatalogueImport(discardLogger(), one.cfg, anAdapter(t), pool)
		if err != nil {
			t.Errorf("%s: newCatalogueImport(): %v", name, err)
			continue
		}
		if imports != nil {
			t.Errorf("%s: an import was scheduled anyway", name)
		}
		if !slices.Equal(missing, one.want) {
			t.Errorf("%s: missing = %v, want %v", name, missing, one.want)
		}
	}
}

// TestAnImportWithNoNetworkIsRefused. Every other missing input is an
// operator's to supply and comes back as a key; a nil adapter is a wiring
// mistake in this file, and the composition root is where it is cheap.
func TestAnImportWithNoNetworkIsRefused(t *testing.T) {
	t.Parallel()
	ctx, pool := opsWiringPool(t)
	_ = ctx

	if _, _, err := newCatalogueImport(discardLogger(), configured(t), nil, pool); err == nil {
		t.Error("an import was built with no network to import from")
	}
}

// TestABrandDirThatHoldsNoBrandStopsTheImport. A deployment that named a
// brand meant it, so an unreadable one is a startup failure rather than an
// import quietly not scheduled - the same rule brandTerms holds.
func TestABrandDirThatHoldsNoBrandStopsTheImport(t *testing.T) {
	t.Parallel()
	ctx, pool := opsWiringPool(t)
	_ = ctx

	cfg := configured(t)
	cfg.BrandDir = t.TempDir()

	if _, _, err := newCatalogueImport(discardLogger(), cfg, anAdapter(t), pool); err == nil {
		t.Error("a BRAND_DIR holding no brand was accepted, so the deployment would start with no import and no explanation")
	}
}

// TestThePublishingBrandComesFromTheBrandFile. brand_id is the tenant
// boundary ADR-0004 draws and it is written once per route, on insert, never
// updated - so a wrong value here is a wrong value for the life of the row,
// and it must come out of the file rather than out of a literal.
func TestThePublishingBrandComesFromTheBrandFile(t *testing.T) {
	t.Parallel()

	dir := brandDir(t)
	raw, err := os.ReadFile(filepath.Join(dir, "brand.json"))
	if err != nil {
		t.Fatalf("reading the brand definition: %v", err)
	}
	publisher, err := brandPublishing(dir)
	if err != nil {
		t.Fatalf("brandPublishing(): %v", err)
	}
	if publisher == "" {
		t.Fatal("the brand fixture named no publisher")
	}
	if !strings.Contains(string(raw), publisher) {
		t.Errorf("brandPublishing() = %q, which is not in the brand file", publisher)
	}

	// And no directory means no brand rather than an empty-string tenant.
	switch none, err := brandPublishing(""); {
	case err != nil:
		t.Errorf("brandPublishing(\"\"): %v", err)
	case none != "":
		t.Errorf("brandPublishing(\"\") = %q, want no brand", none)
	}

	// A directory that holds no brand is an error, and one that names the
	// key an operator set: "no such file" alone would send them looking at
	// the wrong thing.
	switch _, err := brandPublishing(t.TempDir()); {
	case err == nil:
		t.Error("an empty directory was read as a brand")
	case !strings.Contains(err.Error(), "BRAND_DIR="):
		t.Errorf("the error does not name the key that points at it: %v", err)
	}
}
