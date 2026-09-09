// Telling somebody how the jobs are going (ADR-0007).
//
// Every scheduled job in this system passes through one function - runOnce -
// and until now the only thing that happened there was a log line. That was
// enough while a human read `docker logs`; it is not enough to answer "when
// did the settlement sweep last run and how did it go" without one, which is
// what #607 was about to answer with a database table.
//
// So the seam is here rather than in each job: one Observer covers the C-1
// zero-sum check, the earnings lifecycle, the event subscribers, both network
// poll families and the catalogue import, and covers the next job on the day
// it is registered rather than on the day somebody remembers to instrument it.
//
// The interface is declared HERE and not imported from the telemetry package,
// for the reason internal/platform/http declares Instrumentation: the consumer
// defines what it needs, and this package stays ignorant of OpenTelemetry.

package scheduler

import (
	"context"
	"time"
)

// Outcome is what one attempt to run a job came to.
//
// Three outcomes rather than "worked or did not", because the three have
// nothing to do with each other operationally: a job that is failing is an
// incident, a job that is skipping is the fleet lock working exactly as
// designed, and a job that cannot take its lock at all is a database problem
// wearing a scheduler's clothes.
type Outcome string

const (
	// Ran means the job's function was called. Attempt.Err says how it went.
	Ran Outcome = "ran"
	// Skipped means another instance held the lock. With more than one
	// instance this is the MAJORITY outcome, on every tick of every job -
	// which is why it is a counter here and a Debug line in the log.
	Skipped Outcome = "skipped"
	// LockFailed means the lock could not be taken at all. Nothing ran, and
	// the reason is not that somebody else is running it.
	LockFailed Outcome = "lock_failed"
)

// Attempt is what one pass through the scheduler came to, as reported to an
// [Observer].
type Attempt struct {
	// Job is the registered job name, which is bounded and stable across
	// releases - the only thing here safe to use as a metric label.
	Job string
	// Outcome is which of the three happened.
	Outcome Outcome
	// Took is how long the job's function ran. Zero unless Outcome is Ran.
	Took time.Duration
	// Err is what the job returned, or what stopped the lock being taken.
	// Nil on a clean run and on a skip.
	Err error
	// Panicked distinguishes a job that crashed from one that failed. Both
	// arrive as an error, and they are not the same news: a failure is a
	// condition the job reported, a panic is a bug.
	Panicked bool
}

// Observer is told the outcome of every attempt to run a job.
//
// It is called on the scheduler's own goroutine, so an implementation must be
// cheap and must not block - recording a measurement, not writing a row. It
// must also be safe for concurrent use: one goroutine per job calls it.
//
// A panic in an implementation is contained (see [Scheduler.observe]) rather
// than being allowed to kill the job's goroutine. Telemetry that can stop a
// settlement sweep is worse than no telemetry.
type Observer interface {
	JobAttempted(ctx context.Context, attempt Attempt)
}

// observe reports one attempt, tolerating both a nil observer and a broken
// one.
//
// The recover is not defensive decoration. runOnce is called from loop, which
// has no recover of its own: a panic here would unwind the goroutine that
// schedules this job and that job would then never run again for the lifetime
// of the process, silently, because nothing else references that goroutine.
// invoke already draws exactly this line around the job's own function.
func (s *Scheduler) observe(ctx context.Context, attempt Attempt) {
	if s.cfg.Observer == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			s.log.ErrorContext(ctx, "the scheduler's observer panicked and was ignored; job scheduling is unaffected",
				"job", attempt.Job, "outcome", string(attempt.Outcome), "panic", r)
		}
	}()
	s.cfg.Observer.JobAttempted(ctx, attempt)
}
