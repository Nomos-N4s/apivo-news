// Package scheduler runs named jobs in-process on an interval, each one under
// a fleet-wide lock so that two application instances never run the same job
// at the same time.
//
// Exclusion cannot be a property of the process. The api deployment is
// replicated - deploy/k8s/api-hpa.yaml runs minReplicas: 2 - and every
// instance runs this scheduler, so a job guarded only by a mutex would run
// once per instance. It is instead a property of the database: every run
// takes a lock named for the job, and an instance that does not get it skips
// that run rather than waiting for it. Locker is the seam that lock sits
// behind: the composition root wires in the database-backed implementation,
// and the scheduling logic stays testable without one.
//
// # An overrunning job skips, it never queues
//
// Each job gets one goroutine, and that goroutine runs the job to completion
// before it starts waiting again. The interval is therefore the gap between
// the end of one run and the start of the next, not a fixed cadence, and a
// job that overruns its interval delays its next run instead of stacking a
// second one on top of it. Missed ticks are dropped, never accumulated: a job
// that runs long has at most one run in flight per instance, and never a
// backlog to work off once it recovers.
//
// That is the right trade for the jobs this scheduler exists for. An
// affiliate-network poller and the continuous ledger zero-sum check both read
// a window defined by where the last run got to, so two runs queued behind
// each other would do the same work twice, and a backlog built up while a
// network was slow would hammer it the moment it recovered. Skipping is also
// what the lock does across instances, so the in-process rule and the
// fleet-wide rule agree: at most one run of a job at a time, anywhere.
//
// The cost is that a job whose runs take longer than its interval sets its own
// cadence. Bound that with Job.Timeout rather than by queuing.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"runtime/debug"
	"sync"
	"time"
)

const (
	// DefaultJitterFraction is the share of a job's interval by which each
	// wait is moved, up or down. Instances started together - a rolling
	// deployment finishing, a node coming back - would otherwise reach for
	// the same lock in the same instant on every tick, so that all but one
	// of them does nothing but contend. The lock stays correct either way;
	// the jitter is what keeps the contention from being the steady state.
	DefaultJitterFraction = 0.1
	// DefaultShutdownGrace is how long Run waits for in-flight jobs once its
	// context ends, before reporting that it gave up on them.
	DefaultShutdownGrace = 30 * time.Second
	// DefaultReleaseTimeout bounds releasing one job's lock. A release runs
	// on a context detached from the cancelled one - a shutdown must not
	// strand a lock - so it needs a deadline of its own, or an unresponsive
	// connection would hold shutdown open past any grace period.
	DefaultReleaseTimeout = 5 * time.Second
)

// ErrRunning reports an operation that is only legal before Run starts, such
// as registering a job, attempted on a scheduler that is already running.
var ErrRunning = errors.New("scheduler: already running")

// ErrShutdownTimeout reports that Run's shutdown grace expired with at least
// one job still in flight. The abandoned goroutines are still running, so a
// caller should treat it as a reason to end the process, not to carry on.
var ErrShutdownTimeout = errors.New("scheduler: shutdown grace expired with jobs still running")

// PanicError is the error a job's panic is converted into, so that one job's
// bug is reported like any other failure instead of ending the process.
type PanicError struct {
	// Job is the name of the job that panicked.
	Job string
	// Value is the value passed to panic.
	Value any
	// Stack is the stack of the goroutine at the moment of recovery.
	Stack []byte
}

// Error renders the panic value. The stack is carried separately so that it
// is logged as its own attribute rather than buried inside a message.
func (e *PanicError) Error() string {
	return fmt.Sprintf("scheduler: job %q panicked: %v", e.Job, e.Value)
}

// Lock is a held fleet-wide lock.
type Lock interface {
	// Release gives the lock up. The scheduler calls it for every lock it
	// takes - after a successful run, a failed one, a panic, or a
	// cancellation - because a leaked lock silently stops its job for the
	// lifetime of the instance holding it, which is worse than the
	// double-run the lock exists to prevent.
	Release(ctx context.Context) error
}

// Locker hands out the fleet-wide locks that keep two instances from running
// the same job at once. It is the consumer-defined seam over the database,
// so that tests can substitute their own and exercise the scheduling logic
// without one.
type Locker interface {
	// TryLock attempts to take the lock named name without waiting. It
	// reports the held lock and true when it was taken, and nil and false
	// when another instance holds it - an ordinary outcome, not an error.
	// An error means the attempt itself failed.
	TryLock(ctx context.Context, name string) (Lock, bool, error)
}

// Job is one unit of scheduled work.
type Job struct {
	// Name identifies the job in logs and names its fleet-wide lock. It must
	// be unique within a scheduler and stable across releases: two instances
	// exclude each other only if they use the same name.
	Name string
	// Interval is the wait between the end of one run and the start of the
	// next, before jitter. It must be positive.
	Interval time.Duration
	// Timeout bounds one run: the context passed to Run is cancelled after
	// it, which both ends the run and frees the lock for the next tick. Zero
	// means the run is bounded only by the scheduler's own context, so a job
	// that can wedge - anything talking to a network - should set one.
	Timeout time.Duration
	// Run does the work. It is called with a context that ends when the
	// scheduler stops or Timeout expires, and its error is logged, never
	// fatal: the next interval retries from scratch. A panic is recovered
	// and reported as a PanicError.
	Run func(ctx context.Context) error
}

// Config tunes the scheduler. The zero value is usable: every field falls
// back to its documented default.
type Config struct {
	// Jitter is the share of an interval by which each wait is moved, up or
	// down. Zero, or any value outside [0, 1), means DefaultJitterFraction.
	Jitter float64
	// ShutdownGrace bounds how long Run waits for in-flight jobs once its
	// context ends. Zero or negative means DefaultShutdownGrace.
	ShutdownGrace time.Duration
	// ReleaseTimeout bounds releasing one job's lock. Zero or negative means
	// DefaultReleaseTimeout.
	ReleaseTimeout time.Duration
}

// withDefaults returns cfg with every unset or out-of-range field replaced by
// its default.
func (c Config) withDefaults() Config {
	if c.Jitter <= 0 || c.Jitter >= 1 {
		c.Jitter = DefaultJitterFraction
	}
	if c.ShutdownGrace <= 0 {
		c.ShutdownGrace = DefaultShutdownGrace
	}
	if c.ReleaseTimeout <= 0 {
		c.ReleaseTimeout = DefaultReleaseTimeout
	}
	return c
}

// Scheduler runs its registered jobs until its context ends. Construct it with
// New, add jobs with Register, then call Run once.
type Scheduler struct {
	log    *slog.Logger
	locker Locker
	cfg    Config

	mu      sync.Mutex
	jobs    map[string]Job
	started bool

	// random feeds the jitter; a field so tests can pin it.
	random func() float64
}

// New builds a scheduler that logs to log and takes its locks from locker.
// Jobs are registered separately, before Run.
func New(log *slog.Logger, locker Locker, cfg Config) *Scheduler {
	return &Scheduler{
		log:    log,
		locker: locker,
		cfg:    cfg.withDefaults(),
		jobs:   make(map[string]Job),
		//nolint:gosec // G404: the jitter spreads schedules; it guards nothing.
		random: rand.Float64,
	}
}

// Register adds a job. It rejects one that could never run correctly - no
// name, no function, a non-positive interval, a negative timeout, a name
// already taken - and rejects any registration once Run has started, so a late
// call fails loudly instead of being silently ignored.
func (s *Scheduler) Register(job Job) error {
	switch {
	case job.Name == "":
		return errors.New("scheduler: a job needs a name")
	case job.Run == nil:
		return fmt.Errorf("scheduler: job %q: Run must not be nil", job.Name)
	case job.Interval <= 0:
		return fmt.Errorf("scheduler: job %q: interval must be positive, got %v", job.Name, job.Interval)
	case job.Timeout < 0:
		return fmt.Errorf("scheduler: job %q: timeout must not be negative, got %v", job.Name, job.Timeout)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return fmt.Errorf("scheduler: job %q: %w", job.Name, ErrRunning)
	}
	if _, taken := s.jobs[job.Name]; taken {
		return fmt.Errorf("scheduler: job %q is already registered", job.Name)
	}
	s.jobs[job.Name] = job
	return nil
}

// Run drives every registered job until ctx ends, then waits up to the
// configured shutdown grace for the runs still in flight.
//
// It returns nil when every job stopped within the grace and
// ErrShutdownTimeout when one did not. Calling it twice returns ErrRunning; a
// scheduler with no jobs simply waits for ctx, so a deployment that has
// disabled every job still starts.
func (s *Scheduler) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return ErrRunning
	}
	s.started = true
	jobs := make([]Job, 0, len(s.jobs))
	for _, job := range s.jobs {
		jobs = append(jobs, job)
	}
	s.mu.Unlock()

	if len(jobs) == 0 {
		s.log.WarnContext(ctx, "scheduler started with no jobs registered")
		<-ctx.Done()
		return nil
	}

	s.log.InfoContext(ctx, "scheduler started", "jobs", len(jobs))
	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.loop(ctx, job)
		}()
	}

	stopped := make(chan struct{})
	go func() {
		wg.Wait()
		close(stopped)
	}()

	select {
	case <-ctx.Done():
	case <-stopped:
		// Every loop returned without a cancellation. A loop only exits on
		// ctx, so this is unreachable in practice; it is here so that a
		// future exit path cannot turn Run into a permanent block.
		return nil
	}

	grace := time.NewTimer(s.cfg.ShutdownGrace)
	defer grace.Stop()
	select {
	case <-stopped:
		s.log.Info("scheduler stopped")
		return nil
	case <-grace.C:
		s.log.Error("scheduler shutdown grace expired with jobs still running",
			"grace", s.cfg.ShutdownGrace)
		return ErrShutdownTimeout
	}
}

// RunOnce runs one registered job a single time under its lock, reporting
// whether this instance ran it - false means another instance held the lock,
// or taking it failed. It is the single tick Run's loop is built from, exposed
// for callers that bring their own schedule and for tests that need contention
// to be deterministic rather than timed.
func (s *Scheduler) RunOnce(ctx context.Context, name string) (bool, error) {
	s.mu.Lock()
	job, known := s.jobs[name]
	s.mu.Unlock()
	if !known {
		return false, fmt.Errorf("scheduler: no job named %q is registered", name)
	}
	return s.runOnce(ctx, job)
}

// loop is one job's schedule: a first wait inside the jitter window, so that
// instances started together do not all reach for the same lock at once, then
// run and wait until ctx ends.
func (s *Scheduler) loop(ctx context.Context, job Job) {
	if !wait(ctx, startupDelay(job.Interval, s.cfg.Jitter, s.random)) {
		return
	}
	for {
		_, _ = s.runOnce(ctx, job)
		if !wait(ctx, jittered(job.Interval, s.cfg.Jitter, s.random)) {
			return
		}
	}
}

// runOnce takes the job's lock, runs it, and releases the lock. It reports
// whether the job ran here and what it returned; both outcomes are logged, so
// the loop can discard them.
func (s *Scheduler) runOnce(ctx context.Context, job Job) (bool, error) {
	lock, held, err := s.locker.TryLock(ctx, job.Name)
	if err != nil {
		// A lock that cannot be taken is this instance's problem, never
		// fatal: the next interval tries again. During shutdown it is not
		// even that - the attempt failed because ctx ended.
		if ctx.Err() == nil {
			s.log.ErrorContext(ctx, "taking the job lock failed", "job", job.Name, "error", err)
		}
		return false, err
	}
	if !held {
		s.log.InfoContext(ctx, "job skipped: another instance holds its lock", "job", job.Name)
		return false, nil
	}
	defer s.release(ctx, job, lock)

	runCtx := ctx
	if job.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, job.Timeout)
		defer cancel()
	}

	started := time.Now()
	err = invoke(runCtx, job)
	if err != nil {
		var panicked *PanicError
		if errors.As(err, &panicked) {
			s.log.ErrorContext(ctx, "job panicked", "job", job.Name,
				"duration", time.Since(started), "error", err, "stack", string(panicked.Stack))
		} else {
			s.log.ErrorContext(ctx, "job failed", "job", job.Name,
				"duration", time.Since(started), "error", err)
		}
		return true, err
	}
	s.log.InfoContext(ctx, "job completed", "job", job.Name, "duration", time.Since(started))
	return true, nil
}

// release gives the job's lock up on a context detached from ctx, under a
// deadline of its own. Detached because a cancelled context must not stop the
// release - a stranded lock silently stops its job for the lifetime of this
// instance - and deadlined because an unresponsive connection must not hold
// shutdown open past the grace period.
func (s *Scheduler) release(ctx context.Context, job Job, lock Lock) {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.ReleaseTimeout)
	defer cancel()
	if err := lock.Release(releaseCtx); err != nil {
		s.log.ErrorContext(ctx, "releasing the job lock failed", "job", job.Name, "error", err)
	}
}

// invoke calls the job's function with its panic recovered, so a bug in one
// job neither takes the process down nor stops the other jobs - and does not
// stop its own schedule either: the panic becomes an error, the deferred
// release still runs, and the next interval calls the job again.
func invoke(ctx context.Context, job Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &PanicError{Job: job.Name, Value: r, Stack: debug.Stack()}
		}
	}()
	return job.Run(ctx)
}

// wait sleeps for d, reporting true when the whole wait elapsed and false when
// ctx ended first.
func wait(ctx context.Context, d time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// jittered is one between-runs wait: the interval, moved by up to fraction of
// itself either way. random supplies [0, 1).
func jittered(interval time.Duration, fraction float64, random func() float64) time.Duration {
	if interval <= 0 {
		return 0
	}
	offset := (random()*2 - 1) * fraction * float64(interval)
	return interval + time.Duration(offset)
}

// startupDelay is the wait before a job's first run: somewhere in
// [0, fraction x interval). It is deliberately short - a fresh deployment
// should not sit idle for a full interval before its first run - and
// deliberately not zero, so instances started together spread their first
// reach for the lock instead of colliding on it.
func startupDelay(interval time.Duration, fraction float64, random func() float64) time.Duration {
	if interval <= 0 {
		return 0
	}
	return time.Duration(random() * fraction * float64(interval))
}
