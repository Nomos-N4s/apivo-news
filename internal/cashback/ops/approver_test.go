package ops_test

// The stand-in approver every case in this package is built with, and the
// one it uses when an approval must not happen.

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout"
)

// stubApprover answers one payout, or one failure, and records what it was
// asked to approve.
type stubApprover struct {
	paid payout.Payout
	err  error
	// seen is every approval that reached it, so a case can assert WHICH
	// operator the endpoint recorded - the whole of C-4 on this surface.
	seen []payout.Approval
}

func (s *stubApprover) Approve(_ context.Context, approval payout.Approval) (payout.Payout, error) {
	s.seen = append(s.seen, approval)
	if s.err != nil {
		return payout.Payout{}, s.err
	}
	return s.paid, nil
}

// unreachableApprover fails the build of any case that reaches it. Used
// wherever an approval must be refused before the payout module is asked -
// a bad id, a body, a caller who is not an operator.
type unreachableApprover struct{}

func (unreachableApprover) Approve(context.Context, payout.Approval) (payout.Payout, error) {
	return payout.Payout{}, errors.New("ops: this case must not reach the approver")
}

// unreachableRefuser fails any case that reaches it, for the reason
// unreachableApprover does.
type unreachableRefuser struct{}

func (unreachableRefuser) Reject(context.Context, payout.Rejection) (payout.Rejected, error) {
	return payout.Rejected{}, errors.New("ops: this case must not reach the refuser")
}

// unreachableSettler stands where a case must not settle anything.
type unreachableSettler struct{}

func (unreachableSettler) Record(context.Context, payout.Recording) (payout.Settlement, error) {
	return payout.Settlement{}, errors.New("ops: this case must not reach the settler")
}

// unreachableReconciliation stands in for the reconciliation store where a
// case is about something else: every path to it is refused before the
// handler reaches it.
type unreachableReconciliation struct{}

func (unreachableReconciliation) ImportStatement(context.Context, ops.Statement) (ops.ImportedStatement, error) {
	return ops.ImportedStatement{}, errors.New("this case must not import a statement")
}

func (unreachableReconciliation) DetectDifferences(context.Context, uuid.UUID) (ops.Detection, error) {
	return ops.Detection{}, errors.New("this case must not detect differences")
}

func (unreachableReconciliation) ListDifferences(context.Context, uuid.UUID, ops.DifferenceAfter, int) ([]ops.ListedDifference, error) {
	return nil, errors.New("this case must not list differences")
}

func (unreachableReconciliation) ResolveDifference(context.Context, ops.Resolution) (ops.Resolved, error) {
	return ops.Resolved{}, errors.New("this case must not resolve a difference")
}
