package clickout_test

// The contract for POST /clickouts (T072): every status the document
// declares is one the endpoint actually produces, and every status it
// produces is one the document declares.
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
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
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
		for _, raw := range methods {
			var op struct {
				OperationID string                     `json:"operationId"`
				Responses   map[string]json.RawMessage `json:"responses"`
			}
			if err := json.Unmarshal(raw, &op); err != nil || op.OperationID != operationID {
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

// clickOutCase drives the endpoint to one documented status.
type clickOutCase struct {
	name   string
	status int
	// build returns the handler and the request body and token that reach it.
	build func(t *testing.T) (http.Handler, string, string)
}

// clickOutCases covers every status createClickOut declares, one case each.
func clickOutCases(offer func() catalogue.Offer) []clickOutCase {
	body := func(id uuid.UUID) string { return `{"offer_id":"` + id.String() + `"}` }
	live := func(t *testing.T, clicks clickout.ClickStore, deeplinks clickout.Deeplinks) (http.Handler, uuid.UUID) {
		t.Helper()
		o := offer()
		return clickout.NewHandler(discardLogger(),
			issuer(t, &fakeOffers{offer: o}, clicks, deeplinks),
			stubMemberAuth{member: aMember}), o.ID
	}

	return []clickOutCase{
		{
			name: "a tracked redirect is created", status: http.StatusCreated,
			build: func(t *testing.T) (http.Handler, string, string) {
				h, id := live(t, &fakeStore{echo: true}, &fakeDeeplinks{url: "https://x.test/go"})
				return h, body(id), "Bearer t"
			},
		},
		{
			name: "a body naming no offer", status: http.StatusBadRequest,
			build: func(t *testing.T) (http.Handler, string, string) {
				h, _ := live(t, &fakeStore{echo: true}, &fakeDeeplinks{url: "https://x.test/go"})
				return h, `{}`, "Bearer t"
			},
		},
		{
			name: "no usable token", status: http.StatusUnauthorized,
			build: func(t *testing.T) (http.Handler, string, string) {
				o := offer()
				h := clickout.NewHandler(discardLogger(),
					issuer(t, &fakeOffers{offer: o}, &fakeStore{echo: true}, &fakeDeeplinks{url: "https://x.test/go"}),
					stubMemberAuth{err: errors.New("no bearer token")})
				return h, body(o.ID), ""
			},
		},
		{
			name: "the band is not published now", status: http.StatusConflict,
			build: func(t *testing.T) (http.Handler, string, string) {
				h := clickout.NewHandler(discardLogger(),
					issuer(t, &fakeOffers{err: catalogue.ErrOfferNotLive}, &fakeStore{echo: true},
						&fakeDeeplinks{url: "https://x.test/go"}),
					stubMemberAuth{member: aMember})
				return h, body(uuid.New()), "Bearer t"
			},
		},
		{
			name: "the click rule refuses it", status: http.StatusTooManyRequests,
			build: func(t *testing.T) (http.Handler, string, string) {
				o := offer()
				rates := &fakeRates{byAccount: counted(60, clickedAt.Add(-clickout.ClickWindow).Add(time.Second))}
				limit, err := clickout.NewLimiter(rates, clickout.ClickRule{PerMember: 60})
				if err != nil {
					t.Fatalf("NewLimiter(): %v", err)
				}
				h := clickout.NewHandler(discardLogger(),
					issuerWith(t, &fakeOffers{offer: o}, &fakeStore{echo: true},
						&fakeDeeplinks{url: "https://x.test/go"}, clickout.WithLimiter(limit)),
					stubMemberAuth{member: aMember})
				return h, body(o.ID), "Bearer t"
			},
		},
		{
			name: "the click could not be recorded", status: http.StatusInternalServerError,
			build: func(t *testing.T) (http.Handler, string, string) {
				h, id := live(t, &fakeStore{insertErr: errors.New("connection reset")}, &fakeDeeplinks{url: "https://x.test/go"})
				return h, body(id), "Bearer t"
			},
		},
		{
			name: "no redirect could be built", status: http.StatusBadGateway,
			build: func(t *testing.T) (http.Handler, string, string) {
				h, id := live(t, &fakeStore{echo: true}, &fakeDeeplinks{err: networks.ErrDeeplinkNotFormed})
				return h, body(id), "Bearer t"
			},
		},
	}
}

// TestTheClickOutContractIsWhatIsServed drives every case and then compares
// the set of statuses reached with the set the document declares.
func TestTheClickOutContractIsWhatIsServed(t *testing.T) {
	t.Parallel()

	documented := documentedStatuses(t, "createClickOut")
	reached := make([]int, 0, len(documented))

	for _, tc := range clickOutCases(anOffer) {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, requestBody, token := tc.build(t)
			rec := post(t, h, requestBody, token)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.status, rec.Body.String())
			}
			// Every answer but the created one is the single error
			// convention, and a client that cannot parse the failure is a
			// client that cannot report it either.
			if tc.status != http.StatusCreated {
				if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
					t.Errorf("Content-Type = %q, want application/problem+json", got)
				}
				if status, _ := problemOf(t, rec); status != tc.status {
					t.Errorf("the problem body says %d, the response says %d", status, tc.status)
				}
			}
		})
		reached = append(reached, tc.status)
	}

	sort.Ints(reached)
	reached = slices.Compact(reached)

	// The two directions drift for different reasons and both are silent.
	for _, code := range documented {
		if !slices.Contains(reached, code) {
			t.Errorf("the contract declares %d for createClickOut and no case reaches it: either the endpoint cannot produce it, or nothing proves it does", code)
		}
	}
	for _, code := range reached {
		if !slices.Contains(documented, code) {
			t.Errorf("the endpoint answers %d and the contract does not declare it, so a client has no reason to handle it", code)
		}
	}
}
