package catalogue_test

// Exercises the row -> domain mapping with rows supplied directly, no
// database. The store test (store/offers_test.go) already proves the query
// against the migrated schema; what it cannot prove is how the reader
// treats rows the schema forbids - and those are exactly the rows the
// mapping must refuse rather than zero-fill, so they are driven in here by
// hand.

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
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// The reader is wired over the generated store in production; if the seam
// and the generated code drift apart, this fails to compile rather than
// waiting for a wiring error in cmd.
var _ catalogue.OfferStore = (*store.Queries)(nil)

// stubStore hands back one canned row (or error) and records the params it
// was asked with, so a test can assert both halves of the translation.
type stubStore struct {
	row store.GetLiveOfferRow
	err error
	got store.GetLiveOfferParams
}

func (s *stubStore) GetLiveOffer(_ context.Context, arg store.GetLiveOfferParams) (store.GetLiveOfferRow, error) {
	s.got = arg
	return s.row, s.err
}

// Fixed identities, so field-by-field expectations read literally.
var (
	offerID    = uuid.MustParse("5e0e0a4e-9c86-4c53-9f6a-1d2b3c4d5e6f")
	merchantID = uuid.MustParse("a1b2c3d4-e5f6-4a01-8b23-c4d5e6f70819")
	clickedAt  = time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	validFrom  = time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	validTo    = time.Date(2026, time.September, 30, 0, 0, 0, 0, time.UTC)
)

// percentRow is a fully live percent band, the store-row shape of the
// happy path. Every case states its own copy and mutates the one thing
// under test, so the breakage is visible in the table rather than hidden
// in a helper.
func percentRow() store.GetLiveOfferRow {
	return store.GetLiveOfferRow{
		ID:                pgtype.UUID{Bytes: offerID, Valid: true},
		MerchantNetworkID: pgtype.UUID{Bytes: [16]byte{0x01}, Valid: true},
		RateKind:          "percent",
		RateBps:           pgtype.Int4{Int32: 400, Valid: true},
		MemberShareBps:    5000,
		Conditions:        pgtype.Text{String: "new customers only", Valid: true},
		Exclusions:        pgtype.Text{String: "gift cards", Valid: true},
		ValidFrom:         pgtype.Timestamptz{Time: validFrom, Valid: true},
		ValidTo:           pgtype.Timestamptz{Time: validTo, Valid: true},
		DeeplinkTemplate:  "https://network.example/deep?u=https%3A%2F%2Fshop.example",
		NetworkID:         "awin",
		ClickRefParam:     "clickref",
		MerchantID:        pgtype.UUID{Bytes: merchantID, Valid: true},
		MerchantSlug:      "shop-example",
	}
}

// fixedRow is the fixed-band counterpart: a fixed amount with its
// currency, no basis points, and the nullable text columns actually null.
func fixedRow() store.GetLiveOfferRow {
	row := percentRow()
	row.RateKind = "fixed"
	row.RateBps = pgtype.Int4{}
	row.RateFixedMinor = pgtype.Int8{Int64: 250, Valid: true}
	row.Currency = pgtype.Text{String: "EUR", Valid: true}
	row.Conditions = pgtype.Text{}
	row.Exclusions = pgtype.Text{}
	row.ValidTo = pgtype.Timestamptz{}
	return row
}

func TestLiveOfferMapsRows(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		row  store.GetLiveOfferRow
		want catalogue.Offer
	}{
		{
			name: "a percent band comes through whole",
			row:  percentRow(),
			want: catalogue.Offer{
				ID:               offerID,
				MerchantID:       merchantID,
				MerchantSlug:     "shop-example",
				NetworkID:        "awin",
				ClickRefParam:    "clickref",
				Rate:             catalogue.RateBand{Kind: catalogue.RatePercent, Percent: 400},
				MemberShare:      5000,
				Conditions:       "new customers only",
				Exclusions:       "gift cards",
				ValidFrom:        validFrom,
				ValidTo:          validTo,
				DeeplinkTemplate: "https://network.example/deep?u=https%3A%2F%2Fshop.example",
			},
		},
		{
			name: "a fixed band keeps integer minor units and their currency",
			row:  fixedRow(),
			want: catalogue.Offer{
				ID:            offerID,
				MerchantID:    merchantID,
				MerchantSlug:  "shop-example",
				NetworkID:     "awin",
				ClickRefParam: "clickref",
				Rate:          catalogue.RateBand{Kind: catalogue.RateFixed, Fixed: money.Amount{Minor: 250, Currency: "EUR"}},
				MemberShare:   5000,
				// Null conditions, exclusions and valid_to are absence, not
				// empty-string-shaped values: no text, and the zero close.
				ValidFrom:        validFrom,
				DeeplinkTemplate: "https://network.example/deep?u=https%3A%2F%2Fshop.example",
			},
		},
		{
			name: "an infinite valid_to reads as a band with no published end",
			row: func() store.GetLiveOfferRow {
				row := percentRow()
				row.ValidTo = pgtype.Timestamptz{Valid: true, InfinityModifier: pgtype.Infinity}
				return row
			}(),
			want: catalogue.Offer{
				ID:               offerID,
				MerchantID:       merchantID,
				MerchantSlug:     "shop-example",
				NetworkID:        "awin",
				ClickRefParam:    "clickref",
				Rate:             catalogue.RateBand{Kind: catalogue.RatePercent, Percent: 400},
				MemberShare:      5000,
				Conditions:       "new customers only",
				Exclusions:       "gift cards",
				ValidFrom:        validFrom,
				DeeplinkTemplate: "https://network.example/deep?u=https%3A%2F%2Fshop.example",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := &stubStore{row: tc.row}
			got, err := catalogue.NewOfferReader(st).LiveOffer(context.Background(), offerID, clickedAt)
			if err != nil {
				t.Fatalf("LiveOffer: %v", err)
			}
			if got != tc.want {
				t.Errorf("LiveOffer =\n  %+v\nwant\n  %+v", got, tc.want)
			}
		})
	}
}

// The id and moment must reach the store as valid pgtype values: an unset
// Valid flag would make the query match nothing and read as a 409 with no
// cause, which is the kind of bug a test on the seam catches cheaply.
func TestLiveOfferPassesParamsThrough(t *testing.T) {
	t.Parallel()
	st := &stubStore{row: percentRow()}
	if _, err := catalogue.NewOfferReader(st).LiveOffer(context.Background(), offerID, clickedAt); err != nil {
		t.Fatalf("LiveOffer: %v", err)
	}
	if !st.got.ID.Valid || uuid.UUID(st.got.ID.Bytes) != offerID {
		t.Errorf("store asked for id %+v, want %s", st.got.ID, offerID)
	}
	if !st.got.At.Valid || !st.got.At.Time.Equal(clickedAt) {
		t.Errorf("store asked at %+v, want %s", st.got.At, clickedAt)
	}
}

func TestLiveOfferNotLive(t *testing.T) {
	t.Parallel()
	st := &stubStore{err: pgx.ErrNoRows}
	offer, err := catalogue.NewOfferReader(st).LiveOffer(context.Background(), offerID, clickedAt)
	if !errors.Is(err, catalogue.ErrOfferNotLive) {
		t.Fatalf("LiveOffer error = %v, want ErrOfferNotLive", err)
	}
	// The driver's vocabulary must stop at this boundary: a handler that
	// can reach pgx.ErrNoRows through the sentinel will eventually branch
	// on it, and then the 409 is coupled to the database driver.
	if errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("LiveOffer error %v still exposes pgx.ErrNoRows", err)
	}
	if offer != (catalogue.Offer{}) {
		t.Errorf("LiveOffer returned %+v beside the error", offer)
	}
}

func TestLiveOfferReportsStoreFailure(t *testing.T) {
	t.Parallel()
	cause := errors.New("connection reset")
	st := &stubStore{err: cause}
	_, err := catalogue.NewOfferReader(st).LiveOffer(context.Background(), offerID, clickedAt)
	if !errors.Is(err, cause) {
		t.Fatalf("LiveOffer error = %v, want the store's own %v", err, cause)
	}
	// An outage is a 500, never a 409: only the no-rows answer means "not
	// live", so nothing else may wear the sentinel.
	if errors.Is(err, catalogue.ErrOfferNotLive) {
		t.Errorf("LiveOffer dressed a store failure as ErrOfferNotLive: %v", err)
	}
}

// Every way a row can fail the mapping's mirror of offer_rate_kind_fields
// and its neighbours. The schema forbids each of these rows, so seeing one
// means schema and mapping disagree - and the reader must refuse rather
// than hand a zero-filled rate to the click-out flow.
func TestLiveOfferRejectsMalformedRows(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(row *store.GetLiveOfferRow)
	}{
		{"an unknown rate kind", func(row *store.GetLiveOfferRow) {
			row.RateKind = "tiered"
		}},
		{"a percent band with no rate_bps", func(row *store.GetLiveOfferRow) {
			row.RateBps = pgtype.Int4{}
		}},
		{"a percent band carrying a fixed amount", func(row *store.GetLiveOfferRow) {
			row.RateFixedMinor = pgtype.Int8{Int64: 250, Valid: true}
		}},
		{"a percent band carrying a currency", func(row *store.GetLiveOfferRow) {
			row.Currency = pgtype.Text{String: "EUR", Valid: true}
		}},
		{"a rate above one hundred percent", func(row *store.GetLiveOfferRow) {
			row.RateBps = pgtype.Int4{Int32: 10001, Valid: true}
		}},
		{"a negative rate", func(row *store.GetLiveOfferRow) {
			row.RateBps = pgtype.Int4{Int32: -1, Valid: true}
		}},
		{"a fixed band with no amount", func(row *store.GetLiveOfferRow) {
			*row = fixedRow()
			row.RateFixedMinor = pgtype.Int8{}
		}},
		{"a fixed band with no currency", func(row *store.GetLiveOfferRow) {
			*row = fixedRow()
			row.Currency = pgtype.Text{}
		}},
		{"a fixed band with a malformed currency", func(row *store.GetLiveOfferRow) {
			*row = fixedRow()
			row.Currency = pgtype.Text{String: "eur", Valid: true}
		}},
		{"a fixed band of nothing", func(row *store.GetLiveOfferRow) {
			*row = fixedRow()
			row.RateFixedMinor = pgtype.Int8{Int64: 0, Valid: true}
		}},
		{"a fixed band of less than nothing", func(row *store.GetLiveOfferRow) {
			*row = fixedRow()
			row.RateFixedMinor = pgtype.Int8{Int64: -250, Valid: true}
		}},
		{"a fixed band carrying rate_bps", func(row *store.GetLiveOfferRow) {
			*row = fixedRow()
			row.RateBps = pgtype.Int4{Int32: 400, Valid: true}
		}},
		{"a member share above the whole", func(row *store.GetLiveOfferRow) {
			row.MemberShareBps = 10001
		}},
		{"a negative member share", func(row *store.GetLiveOfferRow) {
			row.MemberShareBps = -1
		}},
		{"an unset offer id", func(row *store.GetLiveOfferRow) {
			row.ID = pgtype.UUID{}
		}},
		{"an unset merchant id", func(row *store.GetLiveOfferRow) {
			row.MerchantID = pgtype.UUID{}
		}},
		{"a band with no start", func(row *store.GetLiveOfferRow) {
			row.ValidFrom = pgtype.Timestamptz{}
		}},
		{"a band open since forever", func(row *store.GetLiveOfferRow) {
			// '-infinity' passes the live predicate but pgx hands it over as
			// the zero time - the naive mapping would call it the year 1.
			row.ValidFrom = pgtype.Timestamptz{Valid: true, InfinityModifier: pgtype.NegativeInfinity}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name+" is refused", func(t *testing.T) {
			t.Parallel()
			row := percentRow()
			tc.mutate(&row)
			st := &stubStore{row: row}
			offer, err := catalogue.NewOfferReader(st).LiveOffer(context.Background(), offerID, clickedAt)
			if !errors.Is(err, catalogue.ErrMalformedOffer) {
				t.Fatalf("LiveOffer error = %v, want ErrMalformedOffer", err)
			}
			// A malformed row is a server-side fault, never "not live": the
			// member should see a 500 someone will investigate, not a 409
			// claiming the offer ended.
			if errors.Is(err, catalogue.ErrOfferNotLive) {
				t.Errorf("LiveOffer also wore ErrOfferNotLive: %v", err)
			}
			if offer != (catalogue.Offer{}) {
				t.Errorf("LiveOffer returned %+v beside the error", offer)
			}
		})
	}
}
