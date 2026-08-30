// The tests for status.go: that both closed sets refuse every word outside
// them, and that a route status and a transaction status are not each
// other's vocabulary.

package networks_test

import (
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

func TestParseStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    networks.Status
		wantErr error
	}{
		{name: "pending", raw: "pending", want: networks.StatusPending},
		{name: "confirmed", raw: "confirmed", want: networks.StatusConfirmed},
		{name: "declined", raw: "declined", want: networks.StatusDeclined},
		{name: "reversed", raw: "reversed", want: networks.StatusReversed},
		{name: "a network's own word never parses here", raw: "approved", wantErr: networks.ErrUnmappableStatus},
		{name: "the empty status", raw: "", wantErr: networks.ErrUnmappableStatus},
		{name: "a status differing only in case", raw: "Pending", wantErr: networks.ErrUnmappableStatus},
		{name: "a padded status", raw: "pending ", wantErr: networks.ErrUnmappableStatus},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := networks.ParseStatus(tc.raw)
			portTestAssert(t, "ParseStatus()", err, tc.wantErr, nil)
			if got != tc.want {
				t.Errorf("ParseStatus(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestParseMerchantStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    networks.MerchantStatus
		wantErr error
	}{
		{name: "active", raw: "active", want: networks.MerchantStatusActive},
		{name: "paused", raw: "paused", want: networks.MerchantStatusPaused},
		{name: "left_network", raw: "left_network", want: networks.MerchantStatusLeftNetwork},
		{name: "a network's own word never parses here", raw: "notjoined", wantErr: networks.ErrUnmappableStatus},
		{name: "the empty status", raw: "", wantErr: networks.ErrUnmappableStatus},
		{name: "a route status differing only in case", raw: "Active", wantErr: networks.ErrUnmappableStatus},
		{name: "a padded route status", raw: "active ", wantErr: networks.ErrUnmappableStatus},
		{name: "a transaction status is not a route status", raw: "pending", wantErr: networks.ErrUnmappableStatus},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := networks.ParseMerchantStatus(tc.raw)
			portTestAssert(t, "ParseMerchantStatus()", err, tc.wantErr, nil)
			if got != tc.want {
				t.Errorf("ParseMerchantStatus(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}
