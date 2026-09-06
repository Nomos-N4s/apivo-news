// The Linkwise adapter (T242-T249), starting with what the network is
// willing to answer and at what price.
//
// EVERY NUMBER IN THIS FILE WAS MEASURED, not read from a document. Linkwise
// publishes no rate limit, no paging contract and no maximum query window;
// its API documentation is the usage text a 400 returns. So the values below
// come from probing the live API against a real publisher account on
// 2026-09-04, and the measurements are written down beside them because a
// number whose provenance is lost is a number nobody dares change.
//
// THE FINDING THAT SHAPES EVERYTHING ELSE. Query cost tracks the WIDTH OF THE
// DATE WINDOW, not the amount of data returned. An empty three-month window
// still costs twenty-five seconds. Measured, against windows that returned
// nothing at all:
//
//	  1 day     1.0s        30 days    6.2s
//	  7 days    2.1s        92 days   11.8s
//	                       365 days   81-102s
//
// That is roughly 0.2 seconds per day of window over a fixed second of
// overhead, and it is the opposite of the usual shape, where a big answer is
// what costs. It means the thing to ration here is not requests per minute -
// it is DAYS OF WINDOW PER REQUEST.
//
// A poll of the last year in one call takes a minute and a half and once, in
// testing, exceeded ninety seconds and returned nothing at all. The same
// ground in seven-day slices costs about two seconds a slice, and each slice
// commits its own cursor, so a failure loses one slice rather than the run.
//
// NO PAGING EXISTS. limit, offset, page, per_page, page_size, count, rows,
// max and start were each sent and each ignored - the response came back
// byte-identical every time (3,837,847 bytes for the programme list). The
// date window is therefore the ONLY lever for bounding a response, which is
// the second reason MaxWindow below is small.

// Package linkwise is the adapter for Linkwise, the Greek affiliate network
// (T242-T249).
//
// It declares what the network will answer and at what price. Every number it
// declares was measured against the live API rather than read from a
// document, because Linkwise publishes no rate limit, no paging contract and
// no maximum query window - its API documentation is the usage text a 400
// returns. See the file comment above for the measurements and what they mean.
package linkwise

import (
	"time"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// ID is this network's own identifier: the primary key of its cashback.network
// row, and the value NETWORKS names.
//
// A package constant rather than configuration, for the reason the fixture
// gives for its own: a deployment that could rename it would have rows
// attributed to a network nobody can find the code for.
const ID = networks.NetworkID("linkwise")

// clickRefParam is the query parameter a click-out carries our reference in.
//
// INFERRED, NOT VERIFIED, and it is the one value in this file that was not
// measured. The transaction report returns subid1..subid5 and accepts
// subid1..subid5 as search_field values, so subid1 is what a reference read
// back off a transaction is called. What has NOT been confirmed is that
// subid1 is also the name a TRACKING URL takes it in - Linkwise's creatives
// endpoint returns creative metadata and no tracking URL, so the click side
// could not be read from the API at all.
//
// This is the value the port's own comment warns about: wrong, it silently
// loses attribution on every click. It is stored on cashback.network as a
// column precisely so an operator can correct it without a release, and it
// MUST be confirmed against one real tracking link before a member is ever
// sent through one. Until then the column is a hypothesis with a default.
const clickRefParam = "subid1"

// maxWindow is how much ground one transaction query may cover.
//
// SEVEN DAYS, and this is a latency budget rather than a limit Linkwise
// imposes. The network will answer a year; it simply takes a minute and a
// half to do it, and once took longer than ninety seconds and returned
// nothing. At the measured ~0.2s per day, seven days costs about two seconds
// - comfortably inside any sane HTTP timeout, with room for the day the
// network is slower than it was when this was measured.
//
// The forward sweep runs every fifteen minutes and will almost always ask for
// far less than this; the bound is what protects the TRAILING sweep and the
// first backfill, which are the two that reach for wide windows. A year of
// backfill in seven-day slices is 52 requests of about two seconds - roughly
// the same total as the single 81-second call it replaces, but bounded and
// resumable per slice rather than one request that either returns or does
// not.
const maxWindow = 7 * 24 * time.Hour

// requestsPerMinute is a ceiling this adapter imposes on itself, because
// Linkwise publishes none.
//
// Twelve requests were sent back to back at roughly 1.5 a second: all
// answered 200, none was throttled, none slowed down, and no response carried
// a rate-limit header of any kind - no X-RateLimit-*, no Retry-After, nothing
// to adapt to. Probing was stopped there deliberately. Finding the real
// ceiling would mean hammering a partner's production API with a founder's
// live publisher credentials, and the number is not worth the risk of having
// the account flagged.
//
// So this is conservative by construction, and it costs nothing: at 0.2s per
// day of window, a request is already 1-12 seconds of wall clock, so twenty a
// minute is far above anything the sweeps will actually ask for. It is a
// backstop against a pathological retry loop, not a throughput setting.
//
// It matches Awin's published rate, which is a coincidence worth naming: it
// is not evidence about Linkwise, it is simply a defensible number to pick
// when the network gives you none.
const requestsPerMinute = 20

// reportingLag is zero: Linkwise answers up to the moment.
//
// length=last_2_minutes and length=today both answered in under a second, and
// the recorded transactions show a click at 16:19:02 and its transaction at
// 16:33:58 the same afternoon. That is the merchant reporting quickly, not
// proof of Linkwise's publication lag - but nothing observed suggests a lag,
// and zero is the honest declaration in the absence of evidence rather than a
// guess dressed as one.
//
// If a lag turns up later it is one UPDATE on cashback.network
// (reporting_lag_minutes), with no release: that column exists exactly so a
// value nobody could measure in advance can be corrected from what a
// deployment observes.
const reportingLag = 0

// Limits is what this adapter tells the poller it may ask for.
func Limits() networks.Limits {
	return networks.Limits{
		MaxWindow:         maxWindow,
		RequestsPerMinute: requestsPerMinute,
		ReportingLag:      reportingLag,
	}
}

// Documented is what a cashback.network row is seeded with: the facts this
// network publishes about how it may be queried.
//
// "Publishes" is generous here. Linkwise documents none of these, so what is
// seeded is what was measured - and the row is editable for that reason.
func Documented() networks.Documented {
	limits := Limits()
	return networks.Documented{
		ID:                  ID,
		DisplayName:         "Linkwise",
		ClickRefParam:       clickRefParam,
		MaxQueryWindowDays:  int(limits.MaxWindow / (24 * time.Hour)),
		RateLimitPerMinute:  limits.RequestsPerMinute,
		ReportingLagMinutes: int(limits.ReportingLag / time.Minute),
	}
}
