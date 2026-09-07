// What this deployment is connected to, and why it might be quiet (T228).
//
// A network that reads nothing looks the same in the logs whatever the cause,
// and there are at least five: the binary ships no adapter for it, nobody set
// its credential, the network row or the account row is switched off, its
// backfill has not started yet, or it is deliberately behind by a reporting
// lag. Until this endpoint the way to tell them apart was `docker logs`,
// which is not a surface.
//
// Two of the five answers are NOT in the database. Whether this binary has an
// adapter is a fact about the build, and whether a credential is set is a fact
// about the environment - both belong to the composition root, which is the
// only place allowed to know either (internal/arch/network_isolation_test.go
// rule A). So this module names what it needs and the root supplies it,
// exactly as it does for the ledger and the rails.

package ops

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// ConnectedNetwork is one seeded network and what this deployment can do
// with it.
type ConnectedNetwork struct {
	ID                  string
	DisplayName         string
	ClickRefParam       string
	MaxQueryWindowDays  int
	RateLimitPerMinute  int
	ReportingLagMinutes int
	Active              bool
	// DriverShipped is whether this binary has an adapter for this network.
	//
	// False is FR-092's defect, visible: a row an operator seeded with
	// `connect-network` for a driver the server cannot poll. The deployment
	// refuses to start against it rather than polling nothing, so this
	// answers "why will it not come up" as well as "why is it quiet".
	DriverShipped bool
	Accounts      []ConnectedAccount
}

// ConnectedAccount is one publisher account at that network.
type ConnectedAccount struct {
	ID                  uuid.UUID
	ExternalPublisherID string
	// CredentialRef NAMES A KEY and never holds a value (ADR-0003). It is
	// the string an operator greps their env file for.
	CredentialRef string
	// CredentialPresent is whether that key has anything in it. A boolean
	// and only ever a boolean: not a prefix, not a length, not a redacted
	// form. Any of those would be a fact about a secret leaving the process
	// that holds it.
	CredentialPresent bool
	CursorAt          time.Time
	TrailingCursorAt  time.Time
	BackfillFrom      time.Time
	ReportsCurrency   string
	Active            bool
}

// NetworkInspector answers what this deployment is connected to, named here
// per the boundary rules.
//
// Not satisfied by *PGStore alone, and that is the interesting part: two of
// the fields it must fill are not in the database. The composition root
// composes the store's read with what it knows about the build and the
// environment.
type NetworkInspector interface {
	ConnectedNetworks(ctx context.Context) ([]ConnectedNetwork, error)
}

// networkItem is one network on the wire.
type networkItem struct {
	NetworkID   string `json:"network_id"`
	DisplayName string `json:"display_name"`
	// ClickRefParam is the query parameter a click reference rides in. It is
	// here because a wrong one is the single likeliest cause of a report
	// that matches no click, and the runbook's remedy is an UPDATE on this
	// column - so an operator diagnosing that should be able to read the
	// current value without a psql session.
	ClickRefParam      string `json:"click_ref_param"`
	MaxQueryWindowDays int    `json:"max_query_window_days"`
	RateLimitPerMinute int    `json:"rate_limit_per_minute"`
	// ReportingLagMinutes is how far behind now this network's forward sweep
	// deliberately stays. A non-zero value is why a sale made minutes ago is
	// not here yet, and it is the difference between "behind on purpose" and
	// "stuck".
	ReportingLagMinutes int  `json:"reporting_lag_minutes"`
	Active              bool `json:"active"`
	// DriverShipped false means this binary cannot poll this network at all,
	// whatever else is set.
	DriverShipped bool          `json:"driver_shipped"`
	Accounts      []accountItem `json:"accounts"`
}

// accountItem is one publisher account on the wire.
type accountItem struct {
	AccountID           string `json:"account_id"`
	ExternalPublisherID string `json:"external_publisher_id"`
	CredentialRef       string `json:"credential_ref"`
	CredentialPresent   bool   `json:"credential_present"`
	// CursorAt is how far forward transactions have been fully persisted;
	// TrailingCursorAt is how far the slower re-read has walked. Null on an
	// account that has never completed a window - which on a freshly
	// connected network is the ordinary state and not a fault.
	CursorAt         *string `json:"cursor_at"`
	TrailingCursorAt *string `json:"trailing_cursor_at"`
	// BackfillFrom is where the first sweep starts. In the future, nothing
	// is read until the clock reaches it, which is a fifth way for a network
	// to be correctly configured and completely silent.
	BackfillFrom    *string `json:"backfill_from"`
	ReportsCurrency *string `json:"reports_currency"`
	Active          bool    `json:"active"`
}

// networksPage is every network, with no cursor.
//
// The one operator list that is not paginated. Its size is bounded by what an
// operator seeded and what NETWORKS names - a handful, and a number they
// chose - rather than by member activity, so a cursor would be ceremony. The
// field is still named `items` so the shape matches every other list rather
// than being a bare array somebody has to special-case.
type networksPage struct {
	Items []networkItem `json:"items"`
}

// listNetworks implements GET /api/v1/cashback/ops/networks.
func (h *Handler) listNetworks(w http.ResponseWriter, r *http.Request) {
	if len(r.URL.Query()) > 0 {
		platformhttp.Problem(w, http.StatusBadRequest,
			"this endpoint takes no query parameters; it answers with every network this deployment is connected to")
		return
	}

	networks, err := h.networks.ConnectedNetworks(r.Context())
	if err != nil {
		h.internalError(w, r, "listing the connected networks", err)
		return
	}

	page := networksPage{Items: make([]networkItem, 0, len(networks))}
	for _, network := range networks {
		item := networkItem{
			NetworkID:           network.ID,
			DisplayName:         network.DisplayName,
			ClickRefParam:       network.ClickRefParam,
			MaxQueryWindowDays:  network.MaxQueryWindowDays,
			RateLimitPerMinute:  network.RateLimitPerMinute,
			ReportingLagMinutes: network.ReportingLagMinutes,
			Active:              network.Active,
			DriverShipped:       network.DriverShipped,
			Accounts:            make([]accountItem, 0, len(network.Accounts)),
		}
		for _, account := range network.Accounts {
			item.Accounts = append(item.Accounts, accountItem{
				AccountID:           account.ID.String(),
				ExternalPublisherID: account.ExternalPublisherID,
				CredentialRef:       account.CredentialRef,
				CredentialPresent:   account.CredentialPresent,
				CursorAt:            stampOrNil(account.CursorAt),
				TrailingCursorAt:    stampOrNil(account.TrailingCursorAt),
				BackfillFrom:        stampOrNil(account.BackfillFrom),
				ReportsCurrency:     optional(account.ReportsCurrency),
				Active:              account.Active,
			})
		}
		page.Items = append(page.Items, item)
	}
	h.writeJSON(w, r, page)
}
