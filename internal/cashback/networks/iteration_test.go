// The tests for iteration.go: that an abandoned window says both that the
// answer is partial and why, and that it does not read as a network
// failure.

package networks_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// TestAbandonedIterationSaysTheAnswerIsPartial pins the value contract rule 8
// is satisfied with. It must carry both facts: that the window was not read
// to the end, and why. A caller that could not tell an abandoned window from
// a complete one would advance a durable cursor over transactions it never
// fetched, which is the loss FR-031 forbids.
func TestAbandonedIterationSaysTheAnswerIsPartial(t *testing.T) {
	t.Parallel()

	err := networks.AbandonedIteration(context.Canceled)
	if !errors.Is(err, networks.ErrIterationAbandoned) {
		t.Errorf("AbandonedIteration(ctx err) = %v, want an error wrapping %v", err, networks.ErrIterationAbandoned)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("AbandonedIteration(ctx err) = %v, want it to carry the reason it was handed", err)
	}
	if errors.Is(err, networks.ErrNetworkUnavailable) {
		t.Errorf("AbandonedIteration() = %v, which reads as a network failure rather than a partial answer", err)
	}

	bare := networks.AbandonedIteration(nil)
	if !errors.Is(bare, networks.ErrIterationAbandoned) {
		t.Errorf("AbandonedIteration(nil) = %v, want an error wrapping %v", bare, networks.ErrIterationAbandoned)
	}
}
