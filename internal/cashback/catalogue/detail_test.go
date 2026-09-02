// The faults a merchant page must refuse rather than draw around.
//
// These rows are ones no live database can produce - the schema forbids
// every one of them (0011) - so they are staged against a stub store. That
// is the point: this mapping is the last place where a disagreement between
// the schema and this package is an error somebody can find, instead of a
// rate table a member reads and believes.

package catalogue_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue/store"
)

// stubDetailStore answers the three reads with whatever a case stages.
type stubDetailStore struct {
	merchant    store.CashbackMerchant
	merchantErr error
	copies      []store.CashbackMerchantCopy
	copyErr     error
	bands       []store.PublishedBandsRow
	bandErr     error
}

func (s *stubDetailStore) MerchantBySlug(context.Context, string) (store.CashbackMerchant, error) {
	return s.merchant, s.merchantErr
}

func (s *stubDetailStore) CopyForMerchants(context.Context, []pgtype.UUID) ([]store.CashbackMerchantCopy, error) {
	return s.copies, s.copyErr
}

func (s *stubDetailStore) PublishedBands(context.Context, store.PublishedBandsParams) ([]store.PublishedBandsRow, error) {
	return s.bands, s.bandErr
}

// aStagedMerchant is a well-formed retailer with one German name, so a case
// can break exactly one thing and know that is what it broke.
func aStagedMerchant() *stubDetailStore {
	id := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	return &stubDetailStore{
		merchant: store.CashbackMerchant{
			ID:                 id,
			Slug:               "staged",
			SourceLanguageCode: "de",
			Status:             "active",
		},
		copies: []store.CashbackMerchantCopy{
			{MerchantID: id, LanguageCode: "de", Name: "Bühne"},
		},
	}
}

// aStagedBand is a well-formed 4% commission at half share - so the page
// quotes 2% - for a case to break one field of.
func aStagedBand() store.PublishedBandsRow {
	return store.PublishedBandsRow{
		ID:             pgtype.UUID{Bytes: uuid.New(), Valid: true},
		RateKind:       "percent",
		RateBps:        pgtype.Int4{Int32: 400, Valid: true},
		MemberShareBps: 5000,
		ValidFrom:      pgtype.Timestamptz{Time: detailAt.Add(-time.Hour), Valid: true},
	}
}

// stagedPage builds the reader over a stub.
func stagedPage(t *testing.T, s *stubDetailStore) *catalogue.MerchantReader {
	t.Helper()
	r, err := catalogue.NewMerchantReader(s)
	if err != nil {
		t.Fatalf("NewMerchantReader(): %v", err)
	}
	return r
}

// TestAReaderWithNowhereToReadFromIsRefused. A nil store is a wiring
// mistake, and the composition root is where it is cheap to find.
func TestAReaderWithNowhereToReadFromIsRefused(t *testing.T) {
	if _, err := catalogue.NewMerchantReader(nil); err == nil {
		t.Error("a merchant reader with nowhere to read from was built, want a refusal")
	}
}

// TestTheStagedPageIsWellFormed guards the two helpers above: if the base
// case stopped succeeding, every case below would pass for the wrong
// reason - each one asserts a refusal, and a broken baseline refuses too.
func TestTheStagedPageIsWellFormed(t *testing.T) {
	stub := aStagedMerchant()
	stub.bands = []store.PublishedBandsRow{aStagedBand()}

	page, err := stagedPage(t, stub).Detail(context.Background(), "staged", "de", detailAt)
	if err != nil {
		t.Fatalf("Detail(): %v", err)
	}
	if page.Copy.Name != "Bühne" || len(page.Bands) != 1 || page.Bands[0].Rate.Percent != 200 {
		t.Errorf("staged page = %+v, want one 2%% band - half of a 4%% commission - under the German name", page)
	}
}

// TestAMissingRetailerIsNotADatabaseError. pgx.ErrNoRows stops here, so no
// handler can couple its 404 to the driver by accident.
func TestAMissingRetailerIsNotADatabaseError(t *testing.T) {
	stub := aStagedMerchant()
	stub.merchantErr = pgx.ErrNoRows

	_, err := stagedPage(t, stub).Detail(context.Background(), "staged", "de", detailAt)
	if !errors.Is(err, catalogue.ErrNoMerchant) {
		t.Errorf("Detail() error = %v, want ErrNoMerchant", err)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		t.Error("the driver's own error escaped the package")
	}
}

// TestEveryReadsFailureIsReported. A page assembled out of three reads must
// not answer a failed one with an empty section: no name and no rates are
// both things a member would read as fact.
func TestEveryReadsFailureIsReported(t *testing.T) {
	broken := errors.New("the database fell over")

	for name, stage := range map[string]func(*stubDetailStore){
		"the retailer": func(s *stubDetailStore) { s.merchantErr = broken },
		"the copy":     func(s *stubDetailStore) { s.copyErr = broken },
		"the rates":    func(s *stubDetailStore) { s.bandErr = broken },
	} {
		stub := aStagedMerchant()
		stage(stub)

		_, err := stagedPage(t, stub).Detail(context.Background(), "staged", "de", detailAt)
		if !errors.Is(err, broken) {
			t.Errorf("reading %s failed and Detail() reported %v, want the failure", name, err)
		}
	}
}

// TestARowNoSchemaCouldWriteRefusesThePage. Every case here is forbidden by
// migration 0011, so one arriving means the schema and this mapping
// disagree - and the whole page refuses rather than quoting a rate table
// with a line silently missing from it.
func TestARowNoSchemaCouldWriteRefusesThePage(t *testing.T) {
	unsetID := aStagedBand()
	unsetID.ID = pgtype.UUID{}

	unknownKind := aStagedBand()
	unknownKind.RateKind = "handshake"

	shareBeyondTheWhole := aStagedBand()
	shareBeyondTheWhole.MemberShareBps = 10_001

	noStart := aStagedBand()
	noStart.ValidFrom = pgtype.Timestamptz{}

	closesBeforeAnything := aStagedBand()
	closesBeforeAnything.ValidTo = pgtype.Timestamptz{Valid: true, InfinityModifier: pgtype.NegativeInfinity}

	for name, band := range map[string]store.PublishedBandsRow{
		"a band with no id":               unsetID,
		"a rate kind nothing can compute": unknownKind,
		"a member share beyond the whole": shareBeyondTheWhole,
		"a band that states no start":     noStart,
		"a band that closes at -infinity": closesBeforeAnything,
	} {
		stub := aStagedMerchant()
		stub.bands = []store.PublishedBandsRow{band}

		if _, err := stagedPage(t, stub).Detail(context.Background(), "staged", "de", detailAt); !errors.Is(err, catalogue.ErrMalformedOffer) {
			t.Errorf("%s: Detail() error = %v, want ErrMalformedOffer", name, err)
		}
	}
}

// TestARetailerWithNoIdentityRefuses. An invalid pgtype.UUID converts to
// sixteen zero bytes without complaint, and a page whose click-outs point at
// the zero merchant is exactly that silent zero-fill.
func TestARetailerWithNoIdentityRefuses(t *testing.T) {
	stub := aStagedMerchant()
	stub.merchant.ID = pgtype.UUID{}

	if _, err := stagedPage(t, stub).Detail(context.Background(), "staged", "de", detailAt); !errors.Is(err, catalogue.ErrMalformedOffer) {
		t.Errorf("Detail() error = %v, want ErrMalformedOffer", err)
	}
}
