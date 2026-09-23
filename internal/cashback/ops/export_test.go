package ops

import (
	"context"
	"log/slog"
	"net/http"
)

// Export for testing in ops_test package.

func (h *Handler) RequireOperatorForTest(next http.Handler) http.Handler {
	return h.requireOperator(next)
}

func OperatorFromForTest(ctx context.Context) Operator {
	return operatorFrom(ctx)
}

func NewTestHandler(log *slog.Logger, auth OperatorAuthenticator) *Handler {
	return &Handler{
		log:  log,
		auth: auth,
	}
}
