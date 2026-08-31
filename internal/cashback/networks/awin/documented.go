// What Awin publishes about how its API may be used, in the port's
// vocabulary. One file, because these four facts are a cashback.network row
// and nothing else in this package reads them.

package awin

import "github.com/Nomos-N4s/apivo-news/internal/cashback/networks"

// ClickRefParam is the query parameter Awin reads a publisher's click
// reference from.
//
// Awin also accepts clickref2 through clickref6, and this adapter uses none
// of them: their own documentation says those are "(non-public) parameters
// that cannot be transmitted to the advertiser's landing page", and only
// clickref reaches the retailer.
//
//	https://help.awin.com/developers/docs/click-appends-dyn-params
const ClickRefParam = "clickref"

// MaxQueryWindowDays is the widest transaction window Awin will answer
// (ADR-0003, FR-031). Backfill is windowed because of this number.
const MaxQueryWindowDays = 31

// displayName is what an operator sees. Not member-facing: no member is
// shown which network paid them.
const displayName = "Awin"

// Documented is what Awin publishes about how it may be used, which is what
// a cashback.network row is seeded with when nobody has said otherwise.
//
// It is a function rather than a package variable for the reason the
// fixture's limits are: a value handed out from a shared variable is one any
// caller can edit for every other one, and this one describes a row.
func Documented() networks.Documented {
	return networks.Documented{
		ID:                 ID,
		DisplayName:        displayName,
		ClickRefParam:      ClickRefParam,
		MaxQueryWindowDays: MaxQueryWindowDays,
		RateLimitPerMinute: DocumentedRateLimitPerMinute,
	}
}
