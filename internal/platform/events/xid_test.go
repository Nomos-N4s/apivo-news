package events

// The transaction-id arithmetic behind the checkpoint's no-skip rule.
//
// It gets its own unit test because the case that matters cannot be
// staged against a real database: transaction ids are a 32-bit counter
// that wraps, and reaching the wrap takes four billion transactions. The
// comparison is Postgres's own TransactionIdPrecedes, so what is pinned
// here is that it stays that comparison - a signed distance on a circle -
// and never decays into comparing the two numbers as printed, which would
// judge a freshly wrapped horizon to be older than every row in the
// stream and let the checkpoint run away from an open transaction.

import "testing"

func TestXIDPrecedes(t *testing.T) {
	t.Parallel()

	// The last ids before the counter wraps back to the start.
	const nearWrap = uint32(4294967290)

	tests := []struct {
		name string
		a, b uint32
		want bool
	}{
		{name: "an older id precedes a newer one", a: 100, b: 200, want: true},
		{name: "a newer id does not precede an older one", a: 200, b: 100, want: false},
		{name: "an id does not precede itself", a: 200, b: 200, want: false},
		{name: "an id from before the wrap precedes one from after it", a: nearWrap, b: 5, want: true},
		{name: "an id from after the wrap does not precede one from before it", a: 5, b: nearWrap, want: false},
		{name: "the frozen marker precedes every real transaction", a: 2, b: 100, want: true},
		{name: "no real transaction precedes the frozen marker", a: 100, b: 2, want: false},
		{name: "the frozen marker does not precede itself", a: 2, b: 2, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := xidPrecedes(tt.a, tt.b); got != tt.want {
				t.Fatalf("xidPrecedes(%d, %d) = %t, want %t", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestXID32NarrowsToTheStoredWidth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		read int64
		want uint32
	}{
		{name: "a plain id is itself", read: 12345, want: 12345},
		{name: "the largest id survives", read: 4294967295, want: 4294967295},
		{name: "an id carried past the wrap keeps its stored bits", read: 4294967296 + 7, want: 7},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := xid32(tt.read); got != tt.want {
				t.Fatalf("xid32(%d) = %d, want %d", tt.read, got, tt.want)
			}
		})
	}
}
