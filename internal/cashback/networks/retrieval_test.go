// The tests for retrieval.go: what a retrieval must state before a row is
// written from it, and the refusal that names which part is missing.

package networks_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// retrievalTestAccount is a publisher account of the shape the schema holds:
// an id, a network, and the publisher's own identifier at it.
func retrievalTestAccount(t *testing.T) networks.PublisherAccount {
	t.Helper()
	account, err := networks.NewPublisherAccount(uuid.New(), networks.NetworkID("fixture"), "publisher-1")
	if err != nil {
		t.Fatalf("NewPublisherAccount(): %v", err)
	}
	return account
}

func retrievalTestWindow() networks.QueryWindow {
	return networks.QueryWindow{
		From: time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, time.August, 10, 0, 0, 0, 0, time.UTC),
	}
}

// TestRetrievalNamesTheReadItDescribes holds the three facts an evidence row
// needs and the port deliberately does not carry. Each is a NOT NULL column
// on cashback.network_transaction, so a retrieval missing one produces a row
// the database refuses - discovered halfway through persisting a window,
// after some of it is already permanent.
func TestRetrievalNamesTheReadItDescribes(t *testing.T) {
	t.Parallel()

	account := retrievalTestAccount(t)
	at := time.Date(2026, time.August, 10, 6, 0, 0, 0, time.UTC)

	whole := networks.Retrieval{Account: account, RetrievedAt: at, Window: retrievalTestWindow()}
	if err := whole.Validate(); err != nil {
		t.Fatalf("a retrieval naming all three parts was refused: %v", err)
	}

	missing := map[string]networks.Retrieval{
		"no publisher account": {RetrievedAt: at, Window: retrievalTestWindow()},
		"no moment":            {Account: account, Window: retrievalTestWindow()},
		"no window at all":     {Account: account, RetrievedAt: at},
		"a window with no start": {
			Account:     account,
			RetrievedAt: at,
			Window:      networks.QueryWindow{To: retrievalTestWindow().To},
		},
		"a window ending before it began": {
			Account:     account,
			RetrievedAt: at,
			Window:      networks.QueryWindow{From: retrievalTestWindow().To, To: retrievalTestWindow().From},
		},
	}
	for name, retrieval := range missing {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := retrieval.Validate()
			if !errors.Is(err, networks.ErrInvalidRetrieval) {
				t.Fatalf("Validate() = %v, want one wrapping ErrInvalidRetrieval", err)
			}
		})
	}
}

// TestRetrievalRefusalSaysWhichPartIsAtFault is why the sentinel is its own
// rather than shared with the report sentinels. A bad report is the
// adapter's mistake, in code that translates one network's answer; a bad
// retrieval is the poller's, in code that decides what to ask and when. The
// row either would have written is immutable, so the refusal has to send an
// operator to the right place the first time.
func TestRetrievalRefusalSaysWhichPartIsAtFault(t *testing.T) {
	t.Parallel()

	account := retrievalTestAccount(t)
	at := time.Date(2026, time.August, 10, 6, 0, 0, 0, time.UTC)

	// A retrieval that names no moment must not read as one that names no
	// account, and neither must read as a refused report.
	noMoment := networks.Retrieval{Account: account, Window: retrievalTestWindow()}
	err := noMoment.Validate()
	if !strings.Contains(err.Error(), "moment") {
		t.Errorf("a retrieval with no instant was refused with %q, which does not say so", err)
	}
	if !strings.Contains(err.Error(), account.String()) {
		t.Errorf("the refusal %q does not name the account, so an operator cannot tell which poll wrote it", err)
	}

	noAccount := networks.Retrieval{RetrievedAt: at, Window: retrievalTestWindow()}
	if err := noAccount.Validate(); !errors.Is(err, networks.ErrInvalidPublisherAccount) {
		t.Errorf("a retrieval with no account was refused with %v, want one wrapping ErrInvalidPublisherAccount as well; the cause is what names the part", err)
	}

	badWindow := networks.Retrieval{Account: account, RetrievedAt: at}
	if err := badWindow.Validate(); !errors.Is(err, networks.ErrInvalidQueryWindow) {
		t.Errorf("a retrieval with no window was refused with %v, want one wrapping ErrInvalidQueryWindow as well", err)
	}
}

// TestRetrievalStringNamesThePoll is what an operator reads when a row is
// refused or a window is investigated. All three parts appear: the account
// alone does not say which period, and the period alone does not say which
// account of two at one network.
func TestRetrievalStringNamesThePoll(t *testing.T) {
	t.Parallel()

	account := retrievalTestAccount(t)
	at := time.Date(2026, time.August, 10, 6, 30, 15, 0, time.UTC)
	retrieval := networks.Retrieval{Account: account, RetrievedAt: at, Window: retrievalTestWindow()}

	got := retrieval.String()
	for _, want := range []string{account.String(), retrievalTestWindow().String(), "2026-08-10T06:30:15Z"} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, which does not name %q", got, want)
		}
	}
}

// TestRetrievalStringIsInUTC pins the one formatting decision that matters.
// A poller may run anywhere, and two rows of one window written by two
// processes in two zones would otherwise read as two different instants.
func TestRetrievalStringIsInUTC(t *testing.T) {
	t.Parallel()

	athens, err := time.LoadLocation("Europe/Athens")
	if err != nil {
		t.Skipf("no tz database here: %v", err)
	}
	at := time.Date(2026, time.August, 10, 6, 30, 15, 0, time.UTC)

	utc := networks.Retrieval{Account: retrievalTestAccount(t), RetrievedAt: at, Window: retrievalTestWindow()}
	local := utc
	local.RetrievedAt = at.In(athens)

	if utc.String() != local.String() {
		t.Errorf("the same instant read as\n %s\nand\n %s", utc.String(), local.String())
	}
}
