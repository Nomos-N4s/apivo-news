// FetchTransactions: the window refused before any reading, the recorded
// pages walked in order, and the four ways this iteration can end. One file,
// because contract rules 3, 4, 7, 8 and 9 all land on this one method and are
// only legible together.

package fixture

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strconv"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// FetchTransactions yields every recorded transaction whose transaction time
// falls inside window, at the observation the fixture's clock has reached
// ([Stage]), each already normalised, each carrying the verbatim bytes of the
// recorded response fragment (contract rule 1) and each already through
// [networks.Reported.Validate] (contract rule 7).
//
// The window is refused before anything is read, wrapping
// [networks.ErrWindowTooWide] when it is wider than [Network.Limits] allows -
// never clamped, because a caller that silently received less than it asked
// for would advance its cursor past transactions it never saw (contract rule
// 3, FR-031). That is the whole of the immediate error: everything else is
// yielded, so a caller cannot tell an eager adapter from a lazy one by where
// a failure surfaced (contract rule 9).
//
// Iteration ends in one of four ways, and the difference between them is what
// a caller's cursor depends on:
//
//   - every recorded page was walked and nothing was wrong. The clock then
//     advances, so the next read of the same window answers from the next
//     observation - which is contract rule 4's "the same question, not the
//     same answer", and the only mechanism by which a pending transaction is
//     ever seen to become confirmed;
//   - the caller broke out of the range. The clock does NOT advance, so
//     running that window again from the beginning asks the same question of
//     the same observation and misses nothing. A poller that stopped because
//     a write failed has to be able to do exactly this;
//   - the context was cancelled, or the adapter was told to report a network
//     failure. One pair carrying the error is yielded and iteration returns
//     (contract rules 8 and 9), and the clock does not advance: the answer
//     was not whole, so there is nothing for a cursor to move over;
//   - a recorded status word had no mapping. Yielded as
//     [networks.ErrUnmappableStatus] (contract rule 2) and the clock does not
//     advance, for the same reason.
//
// A range loop that ends having yielded no error therefore means the answer
// was whole, which is what a durable cursor may advance on.
func (n *Network) FetchTransactions(ctx context.Context, window networks.QueryWindow) (iter.Seq2[networks.Reported, error], error) {
	if err := n.limits.ValidateWindow(window); err != nil {
		return nil, fmt.Errorf("fixture: %s: %w", n.account, err)
	}
	return func(yield func(networks.Reported, error) bool) {
		stage := n.clock.now()
		pages := n.recorded.transactionPages(stage, n.unmappable)
		failAt := failurePage(len(pages))

		for index, page := range pages {
			if err := ctx.Err(); err != nil {
				yield(networks.Reported{}, n.abandoned(window, err))
				return
			}
			if index == failAt {
				if err := n.failures.take(); err != nil {
					yield(networks.Reported{}, fmt.Errorf("fixture: %s: window %s: %w", n.account, window, err))
					return
				}
			}
			if !n.yieldPage(ctx, page, window, yield) {
				return
			}
		}
		n.clock.advanceFrom(stage)
	}, nil
}

// yieldPage walks one recorded response body, reporting whether iteration
// should continue. It returns false both when the caller broke and when this
// adapter yielded an error and must stop, because from the loop above those
// are the same instruction - stop, and do not advance the clock.
func (n *Network) yieldPage(ctx context.Context, page transactionPage, window networks.QueryWindow, yield func(networks.Reported, error) bool) bool {
	for _, raw := range page.Transactions {
		if err := ctx.Err(); err != nil {
			yield(networks.Reported{}, n.abandoned(window, err))
			return false
		}
		var recorded recordedTransaction
		if err := json.Unmarshal(raw, &recorded); err != nil {
			yield(networks.Reported{}, fmt.Errorf("%w: page %d of the observation: %w", ErrRecordingUnreadable, page.Page, err))
			return false
		}
		// The window is applied before anything is mapped, because a
		// transaction outside it is not part of this answer at all - so a
		// status word nobody mapped, sitting outside the window somebody
		// asked for, is not this call's problem to report.
		if !window.Contains(recorded.TransactedAt) {
			continue
		}
		report, err := reportFrom(recorded, raw)
		if err != nil {
			yield(networks.Reported{}, err)
			return false
		}
		if !yield(report, nil) {
			return false
		}
	}
	return true
}

// abandoned builds the terminal error contract rule 8 requires when this
// adapter stops before the answer is whole, naming the window so an operator
// reading a log knows which period was left half-read.
func (n *Network) abandoned(window networks.QueryWindow, cause error) error {
	return networks.AbandonedIteration(fmt.Errorf("fixture: %s: window %s: %w", n.account, window, cause))
}

// failurePage is the page index an injected failure strikes at: the second
// page when there is one, and the first when there is not.
//
// Mid-window is deliberate. A failure that always struck before the first
// report would let a caller satisfy contract rule 9 - it classified the
// error, it retried - while still advancing a cursor over a window it had
// read half of, because it would never have been handed a half-read one to
// get wrong. Rule 4 offers no resumption point inside a window, so the only
// correct response to this is to re-run the whole window, and a fixture that
// never produced the situation could not show anybody doing it.
func failurePage(pageCount int) int {
	if pageCount < 2 {
		return 0
	}
	return 1
}

// reportFrom turns one recorded fragment into the port's value: the status
// mapped (contract rule 2), the money rebuilt as minor units carrying an
// explicit currency (C-6), the verbatim bytes carried through (contract rule
// 1), and the whole thing through its own Validate before it can be yielded
// (contract rule 7).
//
// The payload is CLONED rather than shared. The recording is decoded once and
// every adapter in the process reads the same backing array, so a caller
// handed the array itself could edit another test's evidence - and evidence
// that can be edited after the fact is the one thing this whole ingestion
// path exists to prevent.
func reportFrom(recorded recordedTransaction, raw json.RawMessage) (networks.Reported, error) {
	status, err := mapTransactionStatus(recorded.ExternalID, recorded.Status)
	if err != nil {
		return networks.Reported{}, err
	}
	sale, err := recorded.Sale.amount()
	if err != nil {
		return networks.Reported{}, fmt.Errorf("fixture: transaction %s: sale: %w", strconv.Quote(recorded.ExternalID), err)
	}
	commission, err := recorded.Commission.amount()
	if err != nil {
		return networks.Reported{}, fmt.Errorf("fixture: transaction %s: commission: %w", strconv.Quote(recorded.ExternalID), err)
	}
	report := networks.Reported{
		ExternalID:   recorded.ExternalID,
		ClickRef:     recorded.ClickRef,
		StatusRaw:    recorded.Status,
		Status:       status,
		SaleAmount:   sale,
		Commission:   commission,
		TransactedAt: recorded.TransactedAt,
		RawPayload:   clonePayload(raw),
	}
	if err := report.Validate(); err != nil {
		return networks.Reported{}, fmt.Errorf("fixture: %w", err)
	}
	return report, nil
}

// amount rebuilds a recorded figure as a [money.Amount], refusing a currency
// code the money type will not take. There is no scaling step and no parse:
// the recording states minor units, which is what the type holds.
func (a recordedAmount) amount() (money.Amount, error) {
	return money.New(a.MinorUnits, money.Currency(a.Currency))
}
