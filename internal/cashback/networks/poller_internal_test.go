// The two parts of the poller a driven poll cannot reach: the options'
// guards, and the refusal that follows a cursor moving under it.
//
// An internal test file for the same reason poll_internal_test.go is one.
// The options guard unexported fields, and the conditional advance's refusal
// is unreachable through [Poller.PollForward] - the cursor is read FOR
// UPDATE, so nothing that takes the lock can move it in between. Reaching
// that branch means handing advance a store that answers no rows, and only
// a caller inside the package can.

package networks

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/store"
)

// pollerTestCursors answers the two conditional advances with whatever a
// case needs, and keeps what it was asked so the case can assert which
// cursor moved and from where.
type pollerTestCursors struct {
	main     []store.AdvanceNetworkAccountCursorParams
	trailing []store.AdvanceNetworkAccountTrailingCursorParams
	fail     error
}

func (c *pollerTestCursors) AdvanceNetworkAccountCursor(_ context.Context, arg store.AdvanceNetworkAccountCursorParams) (store.AdvanceNetworkAccountCursorRow, error) {
	c.main = append(c.main, arg)
	return store.AdvanceNetworkAccountCursorRow{}, c.fail
}

func (c *pollerTestCursors) AdvanceNetworkAccountTrailingCursor(_ context.Context, arg store.AdvanceNetworkAccountTrailingCursorParams) (store.AdvanceNetworkAccountTrailingCursorRow, error) {
	c.trailing = append(c.trailing, arg)
	return store.AdvanceNetworkAccountTrailingCursorRow{}, c.fail
}

// pollerTestNoDB is a Beginner that would refuse to begin anything. The
// option cases never poll, so it is only here to get past [NewPoller].
type pollerTestNoDB struct{}

func (pollerTestNoDB) Begin(context.Context) (pgx.Tx, error) {
	return nil, errors.New("this poller was never meant to be run")
}

func pollerTestRead() store.GetNetworkAccountCursorsRow {
	return store.GetNetworkAccountCursorsRow{
		CursorAt:         pgtype.Timestamptz{Time: pollTestStart, Valid: true},
		TrailingCursorAt: pgtype.Timestamptz{Time: pollTestStart.Add(-pollTestLag), Valid: true},
		Active:           true,
	}
}

// pollerInternalTestAccount is an account of the shape the schema holds. It
// names no row here: every case in this file stops short of a database.
func pollerInternalTestAccount(t *testing.T) PublisherAccount {
	t.Helper()
	account, err := NewPublisherAccount(uuid.New(), NetworkID("fixture"), "publisher-1")
	if err != nil {
		t.Fatalf("NewPublisherAccount(): %v", err)
	}
	return account
}

func pollerTestWindow() QueryWindow {
	return QueryWindow{From: pollTestStart, To: pollTestStart.Add(pollTestMax)}
}

// TestAdvanceMovesTheCursorItWasWalkingFromWhereItFoundIt is the pair of
// facts a swapped argument would break silently: a forward poll moves the
// main cursor and states the main cursor as the value it must still be at,
// and a trailing poll does the same with its own. Crossing them would move
// the wrong cursor over a window - which is how a period gets skipped.
func TestAdvanceMovesTheCursorItWasWalkingFromWhereItFoundIt(t *testing.T) {
	t.Parallel()

	account := pollerInternalTestAccount(t)
	read, window := pollerTestRead(), pollerTestWindow()

	forward := &pollerTestCursors{}
	if err := (&Poller{}).advance(t.Context(), forward, account, read, window, false); err != nil {
		t.Fatalf("advancing the main cursor: %v", err)
	}
	if len(forward.main) != 1 || len(forward.trailing) != 0 {
		t.Fatalf("a forward poll made %d main and %d trailing advance(s), want 1 and 0",
			len(forward.main), len(forward.trailing))
	}
	if !forward.main[0].AdvanceFrom.Time.Equal(read.CursorAt.Time) {
		t.Errorf("the main advance is conditional on %s, want the cursor it read, %s",
			forward.main[0].AdvanceFrom.Time, read.CursorAt.Time)
	}
	if !forward.main[0].AdvanceTo.Time.Equal(window.To) {
		t.Errorf("the main cursor moved to %s, want the window's end %s",
			forward.main[0].AdvanceTo.Time, window.To)
	}

	trailing := &pollerTestCursors{}
	if err := (&Poller{}).advance(t.Context(), trailing, account, read, window, true); err != nil {
		t.Fatalf("advancing the trailing cursor: %v", err)
	}
	if len(trailing.trailing) != 1 || len(trailing.main) != 0 {
		t.Fatalf("a trailing poll made %d trailing and %d main advance(s), want 1 and 0",
			len(trailing.trailing), len(trailing.main))
	}
	if !trailing.trailing[0].AdvanceFrom.Time.Equal(read.TrailingCursorAt.Time) {
		t.Errorf("the trailing advance is conditional on %s, want the trailing cursor it read, %s",
			trailing.trailing[0].AdvanceFrom.Time, read.TrailingCursorAt.Time)
	}
}

// TestAdvanceReportsACursorThatIsNoLongerWhereItWasRead is the branch the
// FOR UPDATE lock puts out of reach of every test that drives a real poll,
// and the reason [CursorStore] is an interface. No rows is not a database
// failure to investigate: it says this poll's window belongs to somebody
// else's transaction now, and the answer is to stop.
func TestAdvanceReportsACursorThatIsNoLongerWhereItWasRead(t *testing.T) {
	t.Parallel()

	account := pollerInternalTestAccount(t)
	for _, trailing := range []bool{false, true} {
		err := (&Poller{}).advance(t.Context(),
			&pollerTestCursors{fail: pgx.ErrNoRows}, account, pollerTestRead(), pollerTestWindow(), trailing)
		if !errors.Is(err, ErrCursorMoved) {
			t.Errorf("an advance that moved no row (trailing=%t) returned %v, want one wrapping ErrCursorMoved", trailing, err)
		}
	}

	// Everything else is a failure, and must not read as a lost race: a
	// caller that treated a dead connection as "somebody else has this
	// window" would stop polling an account nobody was polling.
	broken := errors.New("connection reset")
	err := (&Poller{}).advance(t.Context(),
		&pollerTestCursors{fail: broken}, account, pollerTestRead(), pollerTestWindow(), false)
	if errors.Is(err, ErrCursorMoved) {
		t.Errorf("a broken connection reported %v, want a failure rather than a lost race", err)
	}
	if !errors.Is(err, broken) {
		t.Errorf("advance() = %v, want one wrapping the cause", err)
	}
}

// TestPollerOptionsIgnoreWhatTheyCannotUse holds the guards on the two
// knobs. A nil clock or a non-positive lag is a caller's mistake, and taking
// either would be worse than ignoring it: a nil clock panics on the first
// poll, and a lag of zero makes the trailing sweep re-read ground the main
// cursor passed a moment ago - which is every window, twice, forever.
func TestPollerOptionsIgnoreWhatTheyCannotUse(t *testing.T) {
	t.Parallel()

	poller, err := NewPoller(pollerTestNoDB{},
		WithPollerClock(nil), WithTrailingLag(0), WithTrailingLag(-time.Hour), nil)
	if err != nil {
		t.Fatalf("NewPoller(): %v", err)
	}
	if poller.now == nil {
		t.Error("a nil clock replaced the real one; the next poll would panic rather than fail")
	}
	if poller.trailingLag != DefaultTrailingLag {
		t.Errorf("the trailing lag is %s, want the default %s", poller.trailingLag, DefaultTrailingLag)
	}

	at := pollTestNow
	poller, err = NewPoller(pollerTestNoDB{},
		WithPollerClock(func() time.Time { return at }), WithTrailingLag(48*time.Hour))
	if err != nil {
		t.Fatalf("NewPoller(): %v", err)
	}
	if !poller.now().Equal(at) {
		t.Errorf("the clock reads %s, want %s", poller.now(), at)
	}
	if poller.trailingLag != 48*time.Hour {
		t.Errorf("the trailing lag is %s, want 48h", poller.trailingLag)
	}
}
