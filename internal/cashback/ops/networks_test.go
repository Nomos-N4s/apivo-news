package ops_test

// What this deployment is connected to, on the wire.
//
// The cases that matter are the ones that distinguish five different reasons
// for a network to be silent, because before this endpoint they were one
// reason: nothing in the logs.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops"
)

// unreachableNetworks is the stand-in every case that must not read this
// surface is given.
type unreachableNetworks struct{}

func (unreachableNetworks) ConnectedNetworks(context.Context) ([]ops.ConnectedNetwork, error) {
	return nil, errors.New("this case must not list networks")
}

// fakeNetworks answers with canned rows.
type fakeNetworks struct {
	networks []ops.ConnectedNetwork
	err      error
}

func (f *fakeNetworks) ConnectedNetworks(context.Context) ([]ops.ConnectedNetwork, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.networks, nil
}

// connected is one fully working network with one account.
func connected(t *testing.T) ops.ConnectedNetwork {
	t.Helper()
	return ops.ConnectedNetwork{
		ID:                  "linkwise",
		DisplayName:         "Linkwise",
		ClickRefParam:       "subid1",
		MaxQueryWindowDays:  31,
		RateLimitPerMinute:  60,
		ReportingLagMinutes: 0,
		Active:              true,
		DriverShipped:       true,
		Accounts: []ops.ConnectedAccount{{
			ID:                  uuid.New(),
			ExternalPublisherID: "CD20",
			CredentialRef:       "config:networks.linkwise.credential",
			CredentialPresent:   true,
			CursorAt:            time.Date(2026, time.September, 7, 12, 0, 0, 0, time.UTC),
			TrailingCursorAt:    time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC),
			BackfillFrom:        time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
			ReportsCurrency:     "EUR",
			Active:              true,
		}},
	}
}

// networksRequest sends an authenticated request to the surface.
func networksRequest(t *testing.T, n ops.NetworkInspector, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, ops.Prefix+path, nil)
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	ops.NewHandler(discardLogger(), &pageStore{}, unreachableApprover{}, unreachableRefuser{},
		unreachableSettler{}, unreachableReconciliation{}, unreachableHeld{}, unreachableDestinations{},
		unreachableAwaiting{}, n, stubAuth{op: anOperator}).ServeHTTP(rec, req)
	return rec
}

// networksBody is the page shape, spelled out so a renamed key fails here.
type networksBody struct {
	Items []struct {
		NetworkID           string `json:"network_id"`
		DisplayName         string `json:"display_name"`
		ClickRefParam       string `json:"click_ref_param"`
		MaxQueryWindowDays  int    `json:"max_query_window_days"`
		RateLimitPerMinute  int    `json:"rate_limit_per_minute"`
		ReportingLagMinutes int    `json:"reporting_lag_minutes"`
		Active              bool   `json:"active"`
		DriverShipped       bool   `json:"driver_shipped"`
		Accounts            []struct {
			AccountID           string  `json:"account_id"`
			ExternalPublisherID string  `json:"external_publisher_id"`
			CredentialRef       string  `json:"credential_ref"`
			CredentialPresent   bool    `json:"credential_present"`
			CursorAt            *string `json:"cursor_at"`
			TrailingCursorAt    *string `json:"trailing_cursor_at"`
			BackfillFrom        *string `json:"backfill_from"`
			ReportsCurrency     *string `json:"reports_currency"`
			Active              bool    `json:"active"`
		} `json:"accounts"`
	} `json:"items"`
}

func readNetworks(t *testing.T, rec *httptest.ResponseRecorder) networksBody {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	var page networksBody
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decoding the page: %v (body %q)", err, rec.Body.String())
	}
	return page
}

// TestTheSurfaceShowsAWorkingNetwork. The ordinary case, so the five
// diagnoses below have something to be different from.
func TestTheSurfaceShowsAWorkingNetwork(t *testing.T) {
	t.Parallel()
	row := connected(t)
	page := readNetworks(t, networksRequest(t, &fakeNetworks{networks: []ops.ConnectedNetwork{row}}, "networks"))

	if len(page.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(page.Items))
	}
	got := page.Items[0]
	if got.NetworkID != row.ID || !got.DriverShipped || !got.Active {
		t.Errorf("network = %+v, want linkwise shipped and active", got)
	}
	if got.ClickRefParam != row.ClickRefParam {
		t.Errorf("click_ref_param = %q, want %q — a wrong one is the likeliest cause of a report matching no click, and its remedy is an UPDATE on this column", got.ClickRefParam, row.ClickRefParam)
	}
	if len(got.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(got.Accounts))
	}
	account := got.Accounts[0]
	if !account.CredentialPresent || account.CursorAt == nil || account.TrailingCursorAt == nil {
		t.Errorf("account = %+v, want a present credential and both cursors", account)
	}
}

// TestTheCredentialIsABooleanAndNothingElse. ADR-0003: the value never
// leaves the process that holds it, and neither does a prefix or a length.
// The ref travels because it NAMES a key, which is what an operator greps
// their env file for.
func TestTheCredentialIsABooleanAndNothingElse(t *testing.T) {
	t.Parallel()
	row := connected(t)
	rec := networksRequest(t, &fakeNetworks{networks: []ops.ConnectedNetwork{row}}, "networks")
	body := rec.Body.String()

	page := readNetworks(t, rec)
	if got := page.Items[0].Accounts[0].CredentialRef; got != row.Accounts[0].CredentialRef {
		t.Errorf("credential_ref = %q, want %q", got, row.Accounts[0].CredentialRef)
	}
	for _, forbidden := range []string{"credential_prefix", "credential_length", "credential_value", "api_key", "api_secret"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the body carries %q; a credential's shape is as much a fact about it as its value", forbidden)
		}
	}
}

// TestASeededNetworkWithNobodyConnected. The LEFT JOIN's null side, and one
// of the five silences: the row exists, the driver may ship, and nothing
// polls because connect-network was never run for a publisher.
func TestASeededNetworkWithNobodyConnected(t *testing.T) {
	t.Parallel()
	row := connected(t)
	row.Accounts = nil
	recorded := networksRequest(t, &fakeNetworks{networks: []ops.ConnectedNetwork{row}}, "networks")
	page := readNetworks(t, recorded)

	if len(page.Items) != 1 {
		t.Fatalf("items = %d, want 1 — a network with no account must still be listed, because that is a state to see", len(page.Items))
	}
	if got := page.Items[0].Accounts; len(got) != 0 {
		t.Errorf("accounts = %v, want an empty list", got)
	}
	// An empty list, not null: a screen that iterates must not have to
	// special-case the network nobody connected.
	if !strings.Contains(recorded.Body.String(), `"accounts":[]`) {
		t.Error(`accounts rendered as null rather than []`)
	}
}

// TestTheFourOtherSilences. A network reading nothing looks the same in the
// logs whatever the cause. Each of these renders differently, which is the
// whole point of the endpoint.
func TestTheFourOtherSilences(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		shape func(*ops.ConnectedNetwork)
		check func(*testing.T, networksBody)
	}{
		{
			name:  "the binary ships no adapter for it",
			shape: func(n *ops.ConnectedNetwork) { n.DriverShipped = false },
			check: func(t *testing.T, p networksBody) {
				if p.Items[0].DriverShipped {
					t.Error("driver_shipped is true for a driver this binary does not have; that is FR-092's defect staying invisible")
				}
			},
		},
		{
			name:  "nobody set its credential",
			shape: func(n *ops.ConnectedNetwork) { n.Accounts[0].CredentialPresent = false },
			check: func(t *testing.T, p networksBody) {
				if p.Items[0].Accounts[0].CredentialPresent {
					t.Error("credential_present is true where no key is set")
				}
			},
		},
		{
			name:  "it is switched off",
			shape: func(n *ops.ConnectedNetwork) { n.Active = false; n.Accounts[0].Active = false },
			check: func(t *testing.T, p networksBody) {
				if p.Items[0].Active || p.Items[0].Accounts[0].Active {
					t.Error("an inactive network or account reads as active")
				}
			},
		},
		{
			name: "it is deliberately behind",
			shape: func(n *ops.ConnectedNetwork) {
				n.ReportingLagMinutes = 240
				n.Accounts[0].CursorAt = time.Time{}
			},
			check: func(t *testing.T, p networksBody) {
				if p.Items[0].ReportingLagMinutes != 240 {
					t.Errorf("reporting_lag_minutes = %d, want 240 — this is what tells 'behind on purpose' from 'stuck'", p.Items[0].ReportingLagMinutes)
				}
				if p.Items[0].Accounts[0].CursorAt != nil {
					t.Error("cursor_at is not null on an account that has never completed a window")
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			row := connected(t)
			c.shape(&row)
			c.check(t, readNetworks(t, networksRequest(t, &fakeNetworks{networks: []ops.ConnectedNetwork{row}}, "networks")))
		})
	}
}

// TestTheSurfaceTakesNoParameters. It answers with everything; a filter
// somebody sent and this ignored would be a view they believe is narrowed.
func TestTheSurfaceTakesNoParameters(t *testing.T) {
	t.Parallel()
	rec := networksRequest(t, unreachableNetworks{}, "networks?network=linkwise")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

// TestTheSurfaceNeedsAnOperator. The gate wraps the whole table; this proves
// the route did not escape it.
func TestTheSurfaceNeedsAnOperator(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodGet, ops.Prefix+"networks", nil)
	rec := httptest.NewRecorder()
	ops.NewHandler(discardLogger(), &pageStore{}, unreachableApprover{}, unreachableRefuser{},
		unreachableSettler{}, unreachableReconciliation{}, unreachableHeld{}, unreachableDestinations{},
		unreachableAwaiting{}, unreachableNetworks{}, stubAuth{err: ops.ErrUnauthenticated}).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// TestAReadThatFailsIsNotAnEmptyDeployment. "Connected to nothing" and "the
// database is gone" must not look the same to somebody diagnosing silence.
func TestAReadThatFailsIsNotAnEmptyDeployment(t *testing.T) {
	t.Parallel()
	rec := networksRequest(t, &fakeNetworks{err: errors.New("the database is gone")}, "networks")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
}
