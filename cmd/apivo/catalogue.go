// Wiring the catalogue import (T105, FR-012).
//
// A file of its own for the reason networks.go is one: it is the piece of
// the composition root that decides whether a job exists at all, and the
// decision has three inputs from three different places - the adapter the
// network configuration produced, the language an operator states, and the
// brand that publishes the routes. Buried in serve it would be a condition
// nobody reads; here it is a function with a name and a test.

package main

import (
	"fmt"
	"log/slog"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/platform/brand"
	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
)

// newCatalogueImport assembles the scheduled catalogue import, or reports
// nil and the environment keys that are still missing.
//
// Missing keys are NOT a startup failure, and that is the same stance the
// network sweeps take on an absent publisher account: importing a catalogue
// needs facts only an operator has, and a deployment configured ahead of
// them should serve everything else rather than refuse to start. What it
// must not do is stay quiet, because an empty catalogue looks exactly like a
// catalogue - so the caller logs at ERROR and names the keys.
//
// A brand directory that is SET and unreadable is a different thing and is
// returned as an error: a deployment that named a brand meant it.
func newCatalogueImport(log *slog.Logger, cfg config.Config, adapter networks.Network, db catalogue.Beginner) (*catalogue.Imports, []string, error) {
	publisher, err := brandPublishing(cfg.BrandDir)
	if err != nil {
		return nil, nil, err
	}

	// The network the root wired, so the language reported missing is the
	// one belonging to the network actually being imported - which is what
	// makes NETWORK_<DRIVER>_SOURCE_LANGUAGE nameable rather than generic.
	network, wired := theConfiguredNetwork(cfg.Cashback)

	var missing []string
	switch {
	case !wired:
		// NETWORKS itself, because there is no driver to name a key after.
		// Reported alongside BRAND_DIR rather than instead of it: a
		// deployment that has neither should learn both in one line, not
		// one per restart.
		missing = append(missing, config.NetworksKey)
	case network.SourceLanguage == "":
		_, _, _, sourceLanguageKey := network.Keys()
		missing = append(missing, sourceLanguageKey)
	}
	if publisher == "" {
		missing = append(missing, "BRAND_DIR")
	}
	if len(missing) > 0 {
		return nil, missing, nil
	}

	importer, err := catalogue.NewImporter(publisher, network.SourceLanguage)
	if err != nil {
		return nil, nil, err
	}
	imports, err := catalogue.NewImports(log, db, adapter, importer)
	if err != nil {
		return nil, nil, err
	}
	return imports, nil, nil
}

// brandPublishing answers which brand publishes the routes an import writes
// (ADR-0004), or "" when this deployment names no brand.
//
// It reads the brand directory a second time rather than sharing what
// brandTerms read, and deliberately says nothing about an unset BRAND_DIR:
// brandTerms already logs that, and one misconfiguration reported twice
// reads as two. Two reads of one directory at start-up cannot disagree -
// they are the same file - so what is duplicated is a few milliseconds and
// not a fact.
func brandPublishing(dir string) (string, error) {
	if dir == "" {
		return "", nil
	}
	defined, err := brand.LoadDir(dir)
	if err != nil {
		return "", fmt.Errorf("BRAND_DIR=%s: %w", dir, err)
	}
	return defined.ID, nil
}
