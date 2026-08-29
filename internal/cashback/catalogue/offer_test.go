package catalogue_test

// Exercises the row -> domain mapping with rows supplied directly, no
// database. The store test (store/offers_test.go) already proves the query
// against the migrated schema; what it cannot prove is how the reader
// treats rows the schema forbids - and those are exactly the rows the
// mapping must refuse rather than zero-fill, so they are driven in here by
// hand.

import (
	"context"
	"encoding/json"
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

// Every combination of the two window ends.
//
// pgx hands 'infinity', '-infinity' and a genuinely unset timestamp over as
// the SAME zero time.Time; only the Valid flag and the InfinityModifier
// tell them apart, so this is the one place where a wrong reading turns
// into a wrong window silently. Nothing below is a preference: each
// expectation is what the store query's own predicates - `o.valid_from <=
// $at` and `coalesce(o.valid_to, 'infinity') > $at` - say about that row,
// so a band the database calls live is mapped rather than refused, and a
// band it rules out is refused rather than published with its meaning
// inverted.
func TestLiveOfferMapsWindowInfinities(t *testing.T) {
	t.Parallel()

	var (
		unset  = pgtype.Timestamptz{}
		before = pgtype.Timestamptz{Time: validFrom, Valid: true}
		after  = pgtype.Timestamptz{Time: validTo, Valid: true}
		always = pgtype.Timestamptz{Valid: true, InfinityModifier: pgtype.NegativeInfinity}
		never  = pgtype.Timestamptz{Valid: true, InfinityModifier: pgtype.Infinity}
	)

	cases := []struct {
		name          string
		from, to      pgtype.Timestamptz
		wantFrom      time.Time
		wantTo        time.Time
		wantMalformed bool
	}{
		{
			name: "a finite window is mapped as it stands",
			from: before, to: after, wantFrom: validFrom, wantTo: validTo,
		},
		{
			name: "a null close is a band with no published end",
			from: before, to: unset, wantFrom: validFrom,
		},
		{
			name: "a '+infinity' close is the same open end as a null one",
			from: before, to: never, wantFrom: validFrom,
		},
		{
			name: "a '-infinity' close is refused: it ends before it can begin",
			from: before, to: always, wantMalformed: true,
		},
		{
			name: "a '-infinity' start is in force since always, and live",
			from: always, to: after, wantTo: validTo,
		},
		{
			name: "in force since always with no published end",
			from: always, to: unset,
		},
		{
			name: "in force since always, open at '+infinity'",
			from: always, to: never,
		},
		{
			name: "in force since always but closing at '-infinity' is still refused",
			from: always, to: always, wantMalformed: true,
		},
		{
			name: "a '+infinity' start is refused: no moment reaches it",
			from: never, to: after, wantMalformed: true,
		},
		{
			name: "a '+infinity' start with a null close is refused too",
			from: never, to: unset, wantMalformed: true,
		},
		{
			name: "a '+infinity' start with an infinite close is refused too",
			from: never, to: never, wantMalformed: true,
		},
		{
			name: "a '+infinity' start with a '-infinity' close is refused twice over",
			from: never, to: always, wantMalformed: true,
		},
		{
			name: "an unset start is refused: valid_from is NOT NULL in the schema",
			from: unset, to: after, wantMalformed: true,
		},
		{
			name: "an unset start with a null close is refused",
			from: unset, to: unset, wantMalformed: true,
		},
		{
			name: "an unset start with an infinite close is refused",
			from: unset, to: never, wantMalformed: true,
		},
		{
			name: "an unset start with a '-infinity' close is refused",
			from: unset, to: always, wantMalformed: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			row := percentRow()
			row.ValidFrom, row.ValidTo = tc.from, tc.to
			got, err := catalogue.NewOfferReader(&stubStore{row: row}).
				LiveOffer(context.Background(), offerID, clickedAt)

			if tc.wantMalformed {
				if !errors.Is(err, catalogue.ErrMalformedOffer) {
					t.Fatalf("LiveOffer error = %v, want ErrMalformedOffer", err)
				}
				// A row the schema forbids is a fault to investigate, not a
				// 409 telling the member the offer ended.
				if errors.Is(err, catalogue.ErrOfferNotLive) {
					t.Errorf("LiveOffer also wore ErrOfferNotLive: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("LiveOffer: %v", err)
			}
			if !got.ValidFrom.Equal(tc.wantFrom) {
				t.Errorf("ValidFrom = %v, want %v", got.ValidFrom, tc.wantFrom)
			}
			if !got.ValidTo.Equal(tc.wantTo) {
				t.Errorf("ValidTo = %v, want %v", got.ValidTo, tc.wantTo)
			}
		})
	}
}

// The band's wire form (FR-013, data-model.md 2.3: click.rate_snapshot is
// jsonb). These are not tests of encoding/json - they are tests that the
// rate a credit is later computed from survives a write and a read
// unchanged, and that a band which does not add up never reaches a snapshot
// at all. Struct encoding gave neither: the unused zero money.Amount on a
// percent band has no currency and refuses to encode, so the commonest kind
// of band did not marshal at all, and a fixed band that did marshal carried
// a "Percent":0 indistinguishable from a real rate of nothing.

// eurAmount is the fixed band's amount, built through money.New so a
// malformed currency fails here rather than inside a case.
func eurAmount(t *testing.T, minor int64) money.Amount {
	t.Helper()
	amount, err := money.New(minor, "EUR")
	if err != nil {
		t.Fatalf("money.New: %v", err)
	}
	return amount
}

func TestRateBandJSONRoundTrip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		band func(t *testing.T) catalogue.RateBand
		want string
	}{
		{
			name: "a percent band carries basis points and no amount",
			band: func(*testing.T) catalogue.RateBand {
				return catalogue.RateBand{Kind: catalogue.RatePercent, Percent: 400}
			},
			want: `{"kind":"percent","bps":400}`,
		},
		{
			name: "a rate of nothing is a rate, not an absence",
			band: func(*testing.T) catalogue.RateBand {
				return catalogue.RateBand{Kind: catalogue.RatePercent, Percent: 0}
			},
			want: `{"kind":"percent","bps":0}`,
		},
		{
			name: "the whole hundred percent survives",
			band: func(*testing.T) catalogue.RateBand {
				return catalogue.RateBand{Kind: catalogue.RatePercent, Percent: money.BasisPointsScale}
			},
			want: `{"kind":"percent","bps":10000}`,
		},
		{
			name: "a fixed band carries minor units with an explicit currency",
			band: func(t *testing.T) catalogue.RateBand {
				return catalogue.RateBand{Kind: catalogue.RateFixed, Fixed: eurAmount(t, 250)}
			},
			want: `{"kind":"fixed","amount":{"minor":250,"currency":"EUR"}}`,
		},
		{
			name: "an amount too large for any float to hold survives exactly",
			band: func(t *testing.T) catalogue.RateBand {
				return catalogue.RateBand{Kind: catalogue.RateFixed, Fixed: eurAmount(t, 9007199254740993)}
			},
			want: `{"kind":"fixed","amount":{"minor":9007199254740993,"currency":"EUR"}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			band := tc.band(t)
			got, err := json.Marshal(band)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("Marshal = %s, want %s", got, tc.want)
			}
			var back catalogue.RateBand
			if err := json.Unmarshal(got, &back); err != nil {
				t.Fatalf("Unmarshal(%s): %v", got, err)
			}
			// RateBand is comparable, so this is the whole value: the kind,
			// the rate AND the field that must have stayed at its zero.
			if back != band {
				t.Errorf("round trip = %+v, want %+v", back, band)
			}
		})
	}
}

// The snapshot is written as one member of a larger object, which is the
// only place the band's encoding actually runs in production - and the
// place struct encoding used to fail the whole write because of the band
// nested inside it.
func TestRateBandRoundTripsInsideASnapshot(t *testing.T) {
	t.Parallel()
	type snapshot struct {
		Rate        catalogue.RateBand `json:"rate"`
		MemberShare money.BasisPoints  `json:"member_share_bps"`
	}
	cases := []struct {
		name string
		in   snapshot
		want string
	}{
		{
			name: "a percent band",
			in:   snapshot{Rate: catalogue.RateBand{Kind: catalogue.RatePercent, Percent: 400}, MemberShare: 5000},
			want: `{"rate":{"kind":"percent","bps":400},"member_share_bps":5000}`,
		},
		{
			name: "a fixed band",
			in: snapshot{
				Rate:        catalogue.RateBand{Kind: catalogue.RateFixed, Fixed: money.Amount{Minor: 250, Currency: "EUR"}},
				MemberShare: 5000,
			},
			want: `{"rate":{"kind":"fixed","amount":{"minor":250,"currency":"EUR"}},"member_share_bps":5000}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := json.Marshal(tc.in)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("Marshal = %s, want %s", got, tc.want)
			}
			var back snapshot
			if err := json.Unmarshal(got, &back); err != nil {
				t.Fatalf("Unmarshal(%s): %v", got, err)
			}
			if back != tc.in {
				t.Errorf("round trip = %+v, want %+v", back, tc.in)
			}
		})
	}
}

// Bands that do not hold to the type's own invariant must not reach a
// snapshot. An error at the write is a bug someone can still find; a
// snapshot with the wrong rate in it is a credit nobody can reconstruct.
func TestRateBandMarshalRefusesIncoherentBands(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		band catalogue.RateBand
	}{
		{"the zero band, which names no kind", catalogue.RateBand{}},
		{"a kind nothing publishes", catalogue.RateBand{Kind: "tiered", Percent: 400}},
		{"a percent band above the whole", catalogue.RateBand{Kind: catalogue.RatePercent, Percent: 10001}},
		{"a percent band below nothing", catalogue.RateBand{Kind: catalogue.RatePercent, Percent: -1}},
		{"a percent band carrying a fixed amount", catalogue.RateBand{
			Kind: catalogue.RatePercent, Percent: 400, Fixed: money.Amount{Minor: 250, Currency: "EUR"},
		}},
		{"a fixed band with no amount at all", catalogue.RateBand{Kind: catalogue.RateFixed}},
		{"a fixed band of nothing", catalogue.RateBand{
			Kind: catalogue.RateFixed, Fixed: money.Amount{Minor: 0, Currency: "EUR"},
		}},
		{"a fixed band of less than nothing", catalogue.RateBand{
			Kind: catalogue.RateFixed, Fixed: money.Amount{Minor: -250, Currency: "EUR"},
		}},
		{"a fixed band with a malformed currency", catalogue.RateBand{
			Kind: catalogue.RateFixed, Fixed: money.Amount{Minor: 250, Currency: "eur"},
		}},
		{"a fixed band carrying basis points", catalogue.RateBand{
			Kind: catalogue.RateFixed, Percent: 400, Fixed: money.Amount{Minor: 250, Currency: "EUR"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name+" does not encode", func(t *testing.T) {
			t.Parallel()
			got, err := json.Marshal(tc.band)
			if !errors.Is(err, catalogue.ErrMalformedOffer) {
				t.Fatalf("Marshal = %s, err = %v; want an error wrapping ErrMalformedOffer", got, err)
			}
		})
	}
}

// Everything a snapshot could come back as that is not a band. Decoding is
// where a rate re-enters the money path years after it was written, so the
// rule the store mapping holds a row to is held here too: exactly the field
// for the stated kind, a whole number of basis points, and nothing else.
func TestRateBandUnmarshalRejects(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		json string
	}{
		{"a JSON null", `null`},
		{"an array", `[{"kind":"percent","bps":400}]`},
		{"a bare string", `"percent"`},
		{"an empty object, which states no kind", `{}`},
		{"a kind nothing publishes", `{"kind":"tiered","bps":400}`},
		{"a kind that is not a string", `{"kind":42,"bps":400}`},
		{"a null kind", `{"kind":null,"bps":400}`},
		{"a percent band with no rate", `{"kind":"percent"}`},
		{"a percent rate with a decimal point", `{"kind":"percent","bps":4.5}`},
		{"a percent rate that is a whole number wearing a decimal point", `{"kind":"percent","bps":400.0}`},
		{"a percent rate in an exponent", `{"kind":"percent","bps":4e2}`},
		{"a percent rate in quotes", `{"kind":"percent","bps":"400"}`},
		{"a percent rate above the whole", `{"kind":"percent","bps":10001}`},
		{"a percent rate below nothing", `{"kind":"percent","bps":-1}`},
		{"a percent band carrying an amount", `{"kind":"percent","bps":400,"amount":{"minor":250,"currency":"EUR"}}`},
		{"a fixed band with no amount", `{"kind":"fixed"}`},
		{"a fixed band carrying basis points", `{"kind":"fixed","bps":400,"amount":{"minor":250,"currency":"EUR"}}`},
		{"a fixed amount of nothing", `{"kind":"fixed","amount":{"minor":0,"currency":"EUR"}}`},
		{"a fixed amount in major units", `{"kind":"fixed","amount":{"minor":2.5,"currency":"EUR"}}`},
		{"a fixed amount with no currency", `{"kind":"fixed","amount":{"minor":250}}`},
		{"a fixed amount that is a bare number", `{"kind":"fixed","amount":250}`},
		{"a field this type does not define", `{"kind":"percent","bps":400,"rate":400}`},
		// Exact key matching, which struct decoding would not give: the
		// second spelling is another field, not another way to say the first.
		{"a key in the wrong case", `{"Kind":"percent","bps":400}`},
		{"a rate given twice", `{"kind":"percent","bps":400,"bps":9999}`},
		{"a kind given twice", `{"kind":"percent","kind":"fixed","bps":400}`},
		{"a second document after the band", `{"kind":"percent","bps":400}{"kind":"fixed"}`},
		{"a stray delimiter after the band", `{"kind":"percent","bps":400}]`},
		{"nothing at all", ``},
	}
	for _, tc := range cases {
		t.Run(tc.name+" is refused", func(t *testing.T) {
			t.Parallel()
			// Seeded with a live band, so a decoder that returns early
			// without writing cannot pass by leaving the zero value behind.
			band := catalogue.RateBand{Kind: catalogue.RatePercent, Percent: 400}
			err := json.Unmarshal([]byte(tc.json), &band)
			if err == nil {
				t.Fatalf("Unmarshal(%s) accepted it as %+v", tc.json, band)
			}
			// A syntax error the standard decoder raises before reaching
			// this type is a rejection too; what must never happen is a
			// silent acceptance.
			t.Logf("rejected with: %v", err)
		})
	}
}

// money.Amount and money.BasisPoints already encode as C-6 requires, so
// this package delegates to them instead of restating their rules. The
// delegation is only as good as what it delegates to, which is what this
// pins: a rate is a whole number, an amount is integer minor units beside
// an explicit currency, and neither is ever a float or a decimal string.
func TestMoneyValuesAlreadyEncodeAsIntegers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		value any
		want  string
	}{
		{"a rate is a whole number of basis points", money.BasisPoints(400), `400`},
		{"the whole is ten thousand", money.BasisPointsScale, `10000`},
		{"an amount is minor units beside its currency", money.Amount{Minor: 250, Currency: "EUR"}, `{"minor":250,"currency":"EUR"}`},
		{"a negative amount keeps its sign", money.Amount{Minor: -250, Currency: "GBP"}, `{"minor":-250,"currency":"GBP"}`},
		{"an amount beyond float64's exact range is exact", money.Amount{Minor: 9007199254740993, Currency: "JPY"}, `{"minor":9007199254740993,"currency":"JPY"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("Marshal = %s, want %s", got, tc.want)
			}
		})
	}
}
