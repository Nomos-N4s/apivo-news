package earnings_test

// The whole transition table, asserted exhaustively (T069, FR-042).
//
// Exhaustively rather than case by case, because the failure this guards is
// an arrow nobody thought about: a legal move that was never written down
// reads as a bug an operator reports, while an ILLEGAL move that slipped in
// reads as money changing state without anybody deciding it should. The
// second is the one that costs, and only a complete table finds it.

import (
	"strings"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings"
)

// states is every state the schema knows, in the order 0013 lists them.
var states = []earnings.State{
	earnings.StateHeld,
	earnings.StatePending,
	earnings.StateConfirmed,
	earnings.StateReserved,
	earnings.StatePaid,
	earnings.StateReversed,
}

// legal is the table written out a second time, independently, as pairs.
// Duplication is the method: a test that read the same map the code reads
// would assert only that a map equals itself, and would stay green if an
// arrow were added or removed.
var legal = map[[2]earnings.State]bool{
	{earnings.StateHeld, earnings.StatePending}:       true,
	{earnings.StatePending, earnings.StateHeld}:       true,
	{earnings.StatePending, earnings.StateConfirmed}:  true,
	{earnings.StateConfirmed, earnings.StateReserved}: true,
	{earnings.StateReserved, earnings.StateConfirmed}: true,
	{earnings.StateReserved, earnings.StatePaid}:      true,
}

// TestEveryPairOfStatesIsDecidedAsTheDesignSays walks all thirty-six ordered
// pairs. Thirty-six is small enough to be complete and complete is the only
// way to catch an arrow that should not exist.
func TestEveryPairOfStatesIsDecidedAsTheDesignSays(t *testing.T) {
	t.Parallel()

	for _, from := range states {
		for _, to := range states {
			want := legal[[2]earnings.State{from, to}]
			if got := earnings.CanFollow(from, to); got != want {
				t.Errorf("CanFollow(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
	}
}

// TestNothingEverBecomesReversed is the arrow whose absence costs the most.
// A reversal is a new entry citing the superseding report and born reversed;
// the credit it undoes is left exactly as it was. entry_guard says the same
// thing in the schema, and the migration's own test refuses a reversal that
// changed the original's state - so an arrow here would be caught by
// Postgres at the worst possible moment instead of by this.
func TestNothingEverBecomesReversed(t *testing.T) {
	t.Parallel()

	for _, from := range states {
		if earnings.CanFollow(from, earnings.StateReversed) {
			t.Errorf("CanFollow(%s, reversed) = true; reversing must create a new entry, not move this one", from)
		}
	}
	if !earnings.CanOpen(earnings.StateReversed) {
		t.Error("CanOpen(reversed) = false; a reversal is born reversed, so it has to be able to start there")
	}
}

// TestNothingFollowsAnEndState is D7's no-edit rule at this layer. Both are
// covered by the exhaustive walk above; they are named separately because
// this is the property, and a reader looking for it should find it as a
// sentence rather than infer it from a table.
func TestNothingFollowsAnEndState(t *testing.T) {
	t.Parallel()

	for _, end := range []earnings.State{earnings.StatePaid, earnings.StateReversed} {
		for _, to := range states {
			if earnings.CanFollow(end, to) {
				t.Errorf("CanFollow(%s, %s) = true; %s is settled and a further transition would edit it", end, to, end)
			}
		}
	}
}

// TestAStateNeverFollowsItself covers what entry_transition_states_differ
// refuses in the schema. Refusing it here too turns a no-op retry into an
// answer the caller gets, rather than a constraint violation from deep
// inside a write that has already moved money.
func TestAStateNeverFollowsItself(t *testing.T) {
	t.Parallel()

	for _, s := range states {
		if earnings.CanFollow(s, s) {
			t.Errorf("CanFollow(%s, %s) = true, want false", s, s)
		}
	}
}

// TestAnEntryOpensOnlyWhereTheNetworkPutIt pins what an entry may be created
// as. Confirmed would mean Apivo decided a commission was final, which is the
// network's to say; reserved and paid describe something that has already
// happened to money the entry has not yet held. Reversed is open because a
// reversal is born there - see TestNothingEverBecomesReversed.
func TestAnEntryOpensOnlyWhereTheNetworkPutIt(t *testing.T) {
	t.Parallel()

	opens := map[earnings.State]bool{
		earnings.StateHeld:     true,
		earnings.StatePending:  true,
		earnings.StateReversed: true,
	}
	for _, s := range states {
		if got := earnings.CanOpen(s); got != opens[s] {
			t.Errorf("CanOpen(%s) = %v, want %v", s, got, opens[s])
		}
	}
}

// TestAnUnknownStateIsNeitherValidNorReachable covers a value that is not one
// of the six. The type is a string, so nothing stops one being built; what
// must not happen is its being treated as a state with no outgoing arrows,
// which would read as "settled" rather than as "not a state".
func TestAnUnknownStateIsNeitherValidNorReachable(t *testing.T) {
	t.Parallel()

	const bogus earnings.State = "settled"
	if bogus.Valid() {
		t.Error("a state the schema does not store reports itself valid")
	}
	for _, s := range states {
		if earnings.CanFollow(bogus, s) {
			t.Errorf("CanFollow(%q, %s) = true", bogus, s)
		}
		if earnings.CanFollow(s, bogus) {
			t.Errorf("CanFollow(%s, %q) = true", s, bogus)
		}
	}
	if earnings.CanOpen(bogus) {
		t.Errorf("CanOpen(%q) = true", bogus)
	}
}

// TestARefusalSaysWhichMoveItRefused covers the message, because a refusal
// nobody can act on without reproducing it is a refusal that gets retried
// blind.
func TestARefusalSaysWhichMoveItRefused(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		err      earnings.ErrIllegalTransition
		contains []string
	}{
		{
			name:     "an ordinary illegal move names both states",
			err:      earnings.ErrIllegalTransition{From: earnings.StateConfirmed, To: earnings.StatePending},
			contains: []string{"confirmed", "pending"},
		},
		{
			name:     "an end state says it is settled",
			err:      earnings.ErrIllegalTransition{From: earnings.StateReversed, To: earnings.StatePending},
			contains: []string{"reversed", "settled"},
		},
		{
			name:     "an unknown source says it is not a state",
			err:      earnings.ErrIllegalTransition{From: "settled", To: earnings.StatePending},
			contains: []string{"settled", "not a state"},
		},
		{
			name:     "an unknown destination says it is not a state",
			err:      earnings.ErrIllegalTransition{From: earnings.StatePending, To: "settled"},
			contains: []string{"settled", "not a state"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.err.Error()
			for _, want := range tc.contains {
				if !strings.Contains(got, want) {
					t.Errorf("message %q does not mention %q", got, want)
				}
			}
		})
	}
}
