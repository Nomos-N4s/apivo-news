package main

// What the scheduler's runs look like from outside the process (ADR-0007).
//
// internal/platform/scheduler declares the Observer interface; this is the one
// implementation, and it lives here rather than in the telemetry package for
// the reason every other adapter in this tree lives at the edge: the
// composition root is where a platform seam is joined to a platform service,
// and neither of the two packages should have to know about the other.
//
// Two instruments and no more. Everything an operator asks about a scheduled
// job - is it running at all, how long does it take, is it failing - is a
// question about one of them, and a third would be a fourth way to derive an
// answer that already has three.

import (
	"context"

	"github.com/Nomos-N4s/apivo-news/internal/platform/scheduler"
	"github.com/Nomos-N4s/apivo-news/internal/platform/telemetry"
)

// jobsScope names the instrumentation scope, so a backend can say where these
// numbers came from.
const jobsScope = "github.com/Nomos-N4s/apivo-news/internal/platform/scheduler"

// Job outcomes as metric labels. Bounded and stable, which is the whole
// requirement for a label: the job name comes from a registration that must
// already be stable across releases to work at all, and these three are the
// scheduler's own enumeration.
const (
	resultOK       = "ok"
	resultFailed   = "failed"
	resultPanicked = "panicked"
)

// jobObserver records what the scheduler did.
type jobObserver struct {
	attempts telemetry.Counter
	duration telemetry.Histogram
}

// newJobObserver builds the observer.
//
// It builds one whether or not this deployment configured telemetry: with it
// off the instruments are no-ops, and the alternative - answering nil, and a
// nil check at the seam - would put a branch in the composition root that only
// ever takes its other side in production. An error means an instrument name
// this binary got wrong, which is a bug rather than a deployment condition.
func newJobObserver(provider *telemetry.Provider) (*jobObserver, error) {
	b := instruments(provider, jobsScope)
	o := &jobObserver{
		attempts: b.counter("apivo.scheduler.job.attempts",
			"Attempts to run a scheduled job, by what the attempt came to."),
		duration: b.histogram("apivo.scheduler.job.duration",
			"How long a scheduled job's run took, for runs that happened.", "s"),
	}
	if b.err != nil {
		return nil, b.err
	}
	return o, nil
}

// JobAttempted records one attempt.
//
// A duration is recorded ONLY for an attempt that ran. An attempt that skipped
// took the time it takes to lose a race for a lock, and putting that on the
// same histogram would mix the wait for a lock into the record of work done -
// which is precisely the number somebody would later read as job latency.
func (o *jobObserver) JobAttempted(ctx context.Context, attempt scheduler.Attempt) {
	o.attempts.Add(ctx, 1,
		telemetry.Label("job", attempt.Job),
		telemetry.Label("outcome", string(attempt.Outcome)))
	if attempt.Outcome != scheduler.Ran {
		return
	}
	o.duration.Record(ctx, attempt.Took.Seconds(),
		telemetry.Label("job", attempt.Job),
		telemetry.Label("result", result(attempt)))
}

// result names how a run that happened turned out.
func result(attempt scheduler.Attempt) string {
	switch {
	case attempt.Panicked:
		return resultPanicked
	case attempt.Err != nil:
		return resultFailed
	default:
		return resultOK
	}
}
