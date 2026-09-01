// The guards on what this module will announce (T100).
//
// Each one refuses a fact the schema would itself have refused, and refuses
// it before the outbox is touched - which is why every case here passes a nil
// database and none of them reaches it. An announcer that appended these
// would put something in the stream that is not in the tables, and a consumer
// reading the stream has no way to find that out.

package payout_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

func TestWhatTheAnnouncerRefusesToSay(t *testing.T) {
	t.Parallel()
	announcer, err := payout.NewAnnouncer()
	if err != nil {
		t.Fatalf("NewAnnouncer(): %v", err)
	}
	ctx := context.Background()
	amount := money.Amount{Minor: 2500, Currency: euros}
	someone := uuid.New()

	for name, announce := range map[string]func() error{
		// A request the database did not insert has no id, so there is
		// nothing for a consumer to look up or order by.
		"a request with no id": func() error {
			return announcer.Requested(ctx, nil, payout.Withdrawal{Member: someone, Amount: amount})
		},
		"an approval with no id": func() error {
			return announcer.Approved(ctx, nil, payout.Withdrawal{Member: someone, Amount: amount, DecidedBy: someone})
		},
		// C-4: approved_by is NOT NULL, so a decision naming nobody is one
		// the schema would have refused. Announcing it would put a decision
		// in the stream that nobody can be asked about.
		"an approval naming no approver": func() error {
			return announcer.Approved(ctx, nil, payout.Withdrawal{ID: uuid.New(), Member: someone, Amount: amount})
		},
		"a rejection with no id": func() error {
			return announcer.Rejected(ctx, nil, payout.Withdrawal{
				Member: someone, Amount: amount, DecidedBy: someone, DecisionReason: "no",
			})
		},
		"a rejection naming no operator": func() error {
			return announcer.Rejected(ctx, nil, payout.Withdrawal{
				ID: uuid.New(), Member: someone, Amount: amount, DecisionReason: "no",
			})
		},
		// withdrawal_request_rejection_has_reason: a refusal without one
		// does not exist to announce, and a member is owed it (FR-061).
		"a rejection with no reason": func() error {
			return announcer.Rejected(ctx, nil, payout.Withdrawal{
				ID: uuid.New(), Member: someone, Amount: amount, DecidedBy: someone,
			})
		},
		"a failure with no payout": func() error {
			return announcer.Failed(ctx, nil, payout.Payout{Request: uuid.New()}, "terminal")
		},
		// Without it a consumer cannot tell a payment that will never happen
		// from one still being retried, which is the only thing this event
		// exists to say.
		"a failure with no classification": func() error {
			return announcer.Failed(ctx, nil, payout.Payout{ID: uuid.New(), Request: uuid.New()}, "")
		},
	} {
		if err := announce(); !errors.Is(err, payout.ErrNotAnnounced) {
			t.Errorf("%s = %v, want one wrapping %v", name, err, payout.ErrNotAnnounced)
		}
	}
}
