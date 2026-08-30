// This file holds the vocabulary's tests: the set of statuses worth
// meeting again, all three answers Retry-After parsing can give, and the
// transparency of the retryable marking itself. Nothing here injects a
// clock or a jitter source, which is the point - these are the assertions
// that need no pacing at all to make, and they are grouped so that the
// contract an adapter codes against can be read on its own.

package networks_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// TestRetryableHTTPStatusClassifies pins the set of statuses worth meeting
// again, including the edges of the 5xx range and the two exceptions
// inside it. 499 is nginx recording that we hung up, which is not a
// statement about the host, and 600 is not a status at all.
func TestRetryableHTTPStatusClassifies(t *testing.T) {
	retryable := []int{
		http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
		599,
	}
	for _, status := range retryable {
		if !networks.RetryableHTTPStatus(status) {
			t.Errorf("RetryableHTTPStatus(%d) = false, want true", status)
		}
	}
	terminal := []int{
		http.StatusOK,
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusUnprocessableEntity,
		499,
		http.StatusNotImplemented,
		600,
	}
	for _, status := range terminal {
		if networks.RetryableHTTPStatus(status) {
			t.Errorf("RetryableHTTPStatus(%d) = true, want false", status)
		}
	}
}

// TestRetryAfterFromHeaderReadsTheAsk covers all three answers the
// signature can give, and the boundary the middle two share. The third -
// present but unreadable - is the reason the error is there: an HTTP-date
// Retry-After is legal and is what several front-ends emit on a 429, and
// collapsing it into the same zero as an absent header leaves an adapter
// retrying under its own half-second base against a host that just asked
// to be left alone for five minutes, with nothing able to count or log it.
func TestRetryAfterFromHeaderReadsTheAsk(t *testing.T) {
	cases := map[string]struct {
		value      string
		want       time.Duration
		unreadable bool
	}{
		"absent":            {value: "", want: 0},
		"whole seconds":     {value: "30", want: 30 * time.Second},
		"padded":            {value: "  2  ", want: 2 * time.Second},
		"fractional":        {value: "1.5", want: 1500 * time.Millisecond},
		"zero":              {value: "0", want: 0},
		"absurd ask capped": {value: "31536000", want: 24 * time.Hour},
		"negative":          {value: "-5", unreadable: true},
		"unparsable":        {value: "soon", unreadable: true},
		"http date":         {value: "Wed, 21 Oct 2015 07:28:00 GMT", unreadable: true},
		"infinite":          {value: "Inf", unreadable: true},
		"not a number":      {value: "NaN", unreadable: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			header := http.Header{}
			if tc.value != "" {
				header.Set("Retry-After", tc.value)
			}
			got, err := networks.RetryAfterFromHeader(header)
			switch {
			case tc.unreadable && !errors.Is(err, networks.ErrRetryAfterUnreadable):
				t.Fatalf("RetryAfterFromHeader(%q) error = %v, want ErrRetryAfterUnreadable; a header we cannot read is not a network asking for nothing, and only the error tells them apart", tc.value, err)
			case !tc.unreadable && err != nil:
				t.Fatalf("RetryAfterFromHeader(%q) error = %v, want nil", tc.value, err)
			}
			if got != tc.want {
				t.Fatalf("RetryAfterFromHeader(%q) = %v, want %v", tc.value, got, tc.want)
			}
			if tc.unreadable && !strings.Contains(err.Error(), tc.value) {
				t.Fatalf("RetryAfterFromHeader(%q) error = %q; an operator cannot act on a refusal that does not say what was refused", tc.value, err)
			}
		})
	}
}

// TestRetryableErrorKeepsItsDiagnosis proves the marking is transparent:
// wrapping a failure to say "try again" must not hide what the failure
// was, or an operator sees a retry verdict and no cause. The message is
// asserted on its content rather than on being non-empty, because a
// constant string is non-empty too and says exactly nothing.
func TestRetryableErrorKeepsItsDiagnosis(t *testing.T) {
	underneath := errors.New("network returned 429")
	marked := networks.NewRetryableError(underneath, 12*time.Second)
	if !errors.Is(marked, underneath) {
		t.Fatalf("errors.Is could not see through the marking to %v", underneath)
	}
	var target *networks.RetryableError
	if !errors.As(marked, &target) || target.RetryAfter() != 12*time.Second {
		t.Fatalf("errors.As did not recover the ask, got %+v", target)
	}
	if got := marked.Error(); !strings.Contains(got, underneath.Error()) || !strings.Contains(got, "12s") {
		t.Fatalf("Error() = %q, want it to name both the ask and the failure underneath", got)
	}

	unasked := networks.NewRetryableError(underneath, 0)
	if got := unasked.Error(); !strings.Contains(got, underneath.Error()) {
		t.Fatalf("Error() = %q, want it to name the failure underneath", got)
	}

	// A negative ask is arithmetic that went wrong somewhere, and a
	// negative wait is a wait that is not taken: it is read as no ask
	// rather than carried into Delay to be added to a jitter.
	backwards := networks.NewRetryableError(underneath, -time.Second)
	var target2 *networks.RetryableError
	if !errors.As(backwards, &target2) || target2.RetryAfter() != 0 {
		t.Fatalf("NewRetryableError(err, -1s) carries an ask of %v, want none", target2.RetryAfter())
	}
}
