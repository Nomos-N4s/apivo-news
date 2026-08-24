package scheduler

// Unit tests for the scheduling logic: the arithmetic of the jitter, the
// registration rules, and every behaviour the loop promises - repetition,
// cancellation, panic isolation, the skip-never-queue rule for an overrunning
// job, and that a lock is released whichever way a run ends.
//
// None of it touches a database. The lock is a Locker the tests supply, which
// is the point of that seam: the only thing that needs a real Postgres is
// whether the advisory lock excludes a second instance, and that is asserted
// in advisorylock_integration_test.go. Everything here runs everywhere.
//
// In-package because the jitter functions and the pinned random source are
// deliberately private - they are pacing details, not API.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
	"github.com/Nomos-N4s/apivo-news/internal/platform/logging"
)

// errLock is the failure a locker under test reports.
var errLock = errors.New("lock unavailable")

// errJob is the failure a job under test reports.
var errJob = errors.New("job failed")

// fakeLocker stands in for the database. It grants every lock unless grant
// says otherwise, and records what was taken and released so a test can assert
// that no run leaves a lock behind.
type fakeLocker struct {
	mu       sync.Mutex
	taken    []string
	released []string

	// grant decides one attempt. Nil grants every attempt.
	grant func(name string) (bool, error)
	// releaseErr, when set, is what every Release reports.
	releaseErr error
}

// TryLock records and answers one attempt.
func (l *fakeLocker) TryLock(_ context.Context, name string) (Lock, bool, error) {
	l.mu.Lock()
	grant := l.grant
	l.mu.Unlock()

	if grant != nil {
		held, err := grant(name)
		if err != nil || !held {
			return nil, false, err
		}
	}

	l.mu.Lock()
	l.taken = append(l.taken, name)
	l.mu.Unlock()
	return &fakeLock{locker: l, name: name}, true, nil
}

// counts reports how many locks have been taken and released.
func (l *fakeLocker) counts() (taken, released int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.taken), len(l.released)
}

// fakeLock is one lock handed out by fakeLocker.
type fakeLock struct {
	locker *fakeLocker
	name   string
}

// Release records the release and reports the locker's configured error.
func (l *fakeLock) Release(context.Context) error {
	l.locker.mu.Lock()
	l.locker.released = append(l.locker.released, l.name)
	err := l.locker.releaseErr
	l.locker.mu.Unlock()
	return err
}

// errCapacity is the refusal a locker under test reports at startup.
var errCapacity = errors.New("the pool cannot seat these jobs")

// capacityLocker is a fakeLocker that also implements CapacityChecker, so the
// startup check has something to ask. fakeLocker deliberately does not
// implement it, which keeps the other tests on the path where a locker has no
// capacity to report.
type capacityLocker struct {
	*fakeLocker
	err error
	// asked records the job count Run passed in.
	asked atomic.Int64
}

// CheckCapacity records the question and gives the configured answer.
func (l *capacityLocker) CheckCapacity(concurrent int) error {
	l.asked.Store(int64(concurrent))
	return l.err
}

// newTestScheduler builds a scheduler with its jitter pinned to the lowest
// draw, so the first run is immediate and every wait is the interval less the
// full jitter swing. The tests below assert on schedules, never on draws.
func newTestScheduler(locker Locker, cfg Config) *Scheduler {
	s := New(slog.New(slog.DiscardHandler), locker, cfg)
	s.random = func() float64 { return 0 }
	return s
}

// start runs s in the background and returns the function that stops it: it
// cancels, waits for Run to return, and reports what Run reported. Calling it
// more than once repeats the first answer.
func start(t *testing.T, s *Scheduler) func() error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- s.Run(ctx) }()

	var (
		once sync.Once
		err  error
	)
	stop := func() error {
		once.Do(func() {
			cancel()
			select {
			case err = <-errc:
			case <-time.After(30 * time.Second):
				err = errors.New("Run did not return within 30s of cancellation")
			}
		})
		return err
	}
	t.Cleanup(func() { cancel() })
	return stop
}

// waitFor blocks until cond holds, failing the test if it never does. It keeps
// the timing tests honest on a loaded runner: they wait for the state they are
// about to assert on instead of sleeping for a guessed interval.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestJitteredIntervalStaysWithinItsBounds(t *testing.T) {
	t.Parallel()

	const interval = 10 * time.Minute
	tests := []struct {
		name     string
		interval time.Duration
		draw     float64
		want     time.Duration
	}{
		{name: "lowest draw is minus the full fraction", interval: interval, draw: 0, want: 9 * time.Minute},
		{name: "middle draw is the interval itself", interval: interval, draw: 0.5, want: interval},
		{name: "highest draw approaches plus the full fraction", interval: interval, draw: 0.9999999, want: 11 * time.Minute},
		{name: "a zero interval waits not at all", interval: 0, draw: 0.5, want: 0},
		{name: "a negative interval waits not at all", interval: -time.Minute, draw: 0.5, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := jittered(tt.interval, DefaultJitterFraction, func() float64 { return tt.draw })
			// A second of slack absorbs the float rounding at the top of
			// the range without letting a real drift through.
			if got < tt.want-time.Second || got > tt.want+time.Second {
				t.Errorf("jittered(%v) with draw %v = %v, want %v", tt.interval, tt.draw, got, tt.want)
			}
		})
	}
}

func TestStartupDelayStaysInsideTheJitterWindow(t *testing.T) {
	t.Parallel()

	const interval = 10 * time.Minute
	tests := []struct {
		name     string
		interval time.Duration
		draw     float64
		want     time.Duration
	}{
		{name: "lowest draw starts at once", interval: interval, draw: 0, want: 0},
		{name: "middle draw starts half a window in", interval: interval, draw: 0.5, want: 30 * time.Second},
		{name: "highest draw approaches the whole window", interval: interval, draw: 0.9999999, want: time.Minute},
		{name: "a zero interval starts at once", interval: 0, draw: 0.9, want: 0},
		{name: "a negative interval starts at once", interval: -time.Minute, draw: 0.9, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := startupDelay(tt.interval, DefaultJitterFraction, func() float64 { return tt.draw })
			if got < 0 || got > time.Duration(DefaultJitterFraction*float64(interval)) {
				t.Errorf("startupDelay(%v) with draw %v = %v, outside [0, %v]",
					tt.interval, tt.draw, got, time.Duration(DefaultJitterFraction*float64(interval)))
			}
			if got < tt.want-time.Second || got > tt.want+time.Second {
				t.Errorf("startupDelay(%v) with draw %v = %v, want %v", tt.interval, tt.draw, got, tt.want)
			}
		})
	}
}

func TestConfigFallsBackToItsDefaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   Config
		want Config
	}{
		{
			name: "the zero value is every default",
			in:   Config{},
			want: Config{Jitter: DefaultJitterFraction, ShutdownGrace: DefaultShutdownGrace, ReleaseTimeout: DefaultReleaseTimeout},
		},
		{
			name: "a jitter of one or more is not a fraction",
			in:   Config{Jitter: 1},
			want: Config{Jitter: DefaultJitterFraction, ShutdownGrace: DefaultShutdownGrace, ReleaseTimeout: DefaultReleaseTimeout},
		},
		{
			name: "a negative jitter is not a fraction",
			in:   Config{Jitter: -0.5},
			want: Config{Jitter: DefaultJitterFraction, ShutdownGrace: DefaultShutdownGrace, ReleaseTimeout: DefaultReleaseTimeout},
		},
		{
			name: "a negative grace is no grace at all",
			in:   Config{ShutdownGrace: -time.Second, ReleaseTimeout: -time.Second},
			want: Config{Jitter: DefaultJitterFraction, ShutdownGrace: DefaultShutdownGrace, ReleaseTimeout: DefaultReleaseTimeout},
		},
		{
			name: "every field set is every field kept",
			in:   Config{Jitter: 0.25, ShutdownGrace: time.Minute, ReleaseTimeout: time.Second},
			want: Config{Jitter: 0.25, ShutdownGrace: time.Minute, ReleaseTimeout: time.Second},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.in.withDefaults(); got != tt.want {
				t.Errorf("Config%+v.withDefaults() = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestRegisterRejectsAJobThatCouldNotRun(t *testing.T) {
	t.Parallel()

	noop := func(context.Context) error { return nil }
	tests := []struct {
		name string
		job  Job
		want string
	}{
		{name: "no name", job: Job{Interval: time.Second, Run: noop}, want: "needs a name"},
		{name: "no function", job: Job{Name: "j", Interval: time.Second}, want: "Run must not be nil"},
		{name: "zero interval", job: Job{Name: "j", Run: noop}, want: "interval must be positive"},
		{name: "negative interval", job: Job{Name: "j", Interval: -time.Second, Run: noop}, want: "interval must be positive"},
		{name: "negative timeout", job: Job{Name: "j", Interval: time.Second, Timeout: -time.Second, Run: noop}, want: "timeout must not be negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := newTestScheduler(&fakeLocker{}, Config{}).Register(tt.job)
			if err == nil {
				t.Fatalf("Register(%+v): want an error, got nil", tt.job)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Register(%+v) error = %q, want it to mention %q", tt.job, err, tt.want)
			}
		})
	}
}

func TestRegisterRejectsANameAlreadyTaken(t *testing.T) {
	t.Parallel()

	s := newTestScheduler(&fakeLocker{}, Config{})
	job := Job{Name: "poll", Interval: time.Second, Run: func(context.Context) error { return nil }}
	if err := s.Register(job); err != nil {
		t.Fatalf("first Register() error: %v", err)
	}
	err := s.Register(job)
	if err == nil {
		t.Fatal("second Register() with the same name: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "already registered") {
		t.Errorf("second Register() error = %q, want it to mention that the name is taken", err)
	}
}

func TestRegisterAndRunAreRefusedOnceRunning(t *testing.T) {
	t.Parallel()

	ran := make(chan struct{})
	var once sync.Once
	s := newTestScheduler(&fakeLocker{}, Config{})
	if err := s.Register(Job{
		Name:     "steady",
		Interval: time.Millisecond,
		Run: func(context.Context) error {
			once.Do(func() { close(ran) })
			return nil
		},
	}); err != nil {
		t.Fatalf("Register() error: %v", err)
	}
	stop := start(t, s)

	select {
	case <-ran:
	case <-time.After(30 * time.Second):
		t.Fatal("the registered job never ran")
	}

	err := s.Register(Job{Name: "late", Interval: time.Second, Run: func(context.Context) error { return nil }})
	if !errors.Is(err, ErrRunning) {
		t.Errorf("Register() after Run started = %v, want ErrRunning", err)
	}
	if err := s.Run(context.Background()); !errors.Is(err, ErrRunning) {
		t.Errorf("second Run() = %v, want ErrRunning", err)
	}
	if err := stop(); err != nil {
		t.Errorf("Run() error: %v", err)
	}
}

func TestRunRefusesToStartWhenTheLockerCannotSeatItsJobs(t *testing.T) {
	t.Parallel()

	var runs atomic.Int64
	locker := &capacityLocker{fakeLocker: &fakeLocker{}, err: errCapacity}
	s := newTestScheduler(locker, Config{})
	for _, name := range []string{"network-poll", "ledger-zero-sum"} {
		if err := s.Register(Job{
			Name:     name,
			Interval: time.Millisecond,
			Run: func(context.Context) error {
				runs.Add(1)
				return nil
			},
		}); err != nil {
			t.Fatalf("Register(%s) error: %v", name, err)
		}
	}

	// A live context: Run must refuse and return on its own, not wait to be
	// cancelled. A misconfiguration should stop the deployment.
	if err := s.Run(context.Background()); !errors.Is(err, errCapacity) {
		t.Errorf("Run() with a locker that cannot seat its jobs = %v, want errCapacity", err)
	}
	if got := locker.asked.Load(); got != 2 {
		t.Errorf("CheckCapacity() was asked about %d jobs, want 2: it must be told what is registered", got)
	}
	if got := runs.Load(); got != 0 {
		t.Errorf("%d jobs ran, want 0: nothing may start when the pool cannot seat them", got)
	}
	if taken, _ := locker.counts(); taken != 0 {
		t.Errorf("%d locks were taken, want 0", taken)
	}
}

func TestRunStartsWhenTheLockerReportsCapacity(t *testing.T) {
	t.Parallel()

	var runs atomic.Int64
	locker := &capacityLocker{fakeLocker: &fakeLocker{}}
	s := newTestScheduler(locker, Config{})
	if err := s.Register(Job{
		Name:     "network-poll",
		Interval: time.Millisecond,
		Run: func(context.Context) error {
			runs.Add(1)
			return nil
		},
	}); err != nil {
		t.Fatalf("Register() error: %v", err)
	}
	stop := start(t, s)

	waitFor(t, "the job to run", func() bool { return runs.Load() >= 1 })
	if err := stop(); err != nil {
		t.Errorf("Run() error: %v", err)
	}
	if got := locker.asked.Load(); got != 1 {
		t.Errorf("CheckCapacity() was asked about %d jobs, want 1", got)
	}
}

func TestRunWithoutJobsWaitsForCancellation(t *testing.T) {
	t.Parallel()

	s := newTestScheduler(&fakeLocker{}, Config{})
	stop := start(t, s)
	if err := stop(); err != nil {
		t.Errorf("Run() with no jobs = %v, want nil", err)
	}
}

func TestSchedulerRunsAJobRepeatedlyUntilCancelled(t *testing.T) {
	t.Parallel()

	var runs atomic.Int64
	locker := &fakeLocker{}
	s := newTestScheduler(locker, Config{})
	if err := s.Register(Job{
		Name:     "poll",
		Interval: time.Millisecond,
		Run: func(context.Context) error {
			runs.Add(1)
			return nil
		},
	}); err != nil {
		t.Fatalf("Register() error: %v", err)
	}
	stop := start(t, s)

	waitFor(t, "the job to run three times", func() bool { return runs.Load() >= 3 })
	if err := stop(); err != nil {
		t.Errorf("Run() error: %v", err)
	}

	// Nothing may still be scheduled once Run has returned.
	settled := runs.Load()
	time.Sleep(20 * time.Millisecond)
	if got := runs.Load(); got != settled {
		t.Errorf("the job ran %d more times after Run returned; cancellation must stop the schedule", got-settled)
	}

	taken, released := locker.counts()
	if taken != released {
		t.Errorf("locks taken = %d, released = %d; every run must release its lock", taken, released)
	}
}

// TestTheScheduledWaitsAreJittered pins the wiring, not the arithmetic:
// startupDelay and jittered are proved correct as functions elsewhere, and
// this asserts that loop actually calls them. Without it, waiting for a flat
// zero before the first run and a flat interval between runs passes every
// other test in this file - and every replica would then reach for the lock in
// the same instant on every tick and after every rolling restart, which is the
// contention the jitter exists to prevent.
//
// The jitter is set to half the interval and the draw pinned to the top of the
// range, which puts both waits far enough from their unjittered values to be
// told apart on a loaded runner. Both timing assertions are lower bounds, so
// load can only make them safer; the count of draws is exact.
func TestTheScheduledWaitsAreJittered(t *testing.T) {
	t.Parallel()

	const (
		interval = 300 * time.Millisecond
		jitter   = 0.5
		// startupDelay(300ms, 0.5, ~1) approaches 150ms, and a first run
		// that did not wait at all lands within a millisecond or two.
		wantStartupAtLeast = 100 * time.Millisecond
		// jittered(300ms, 0.5, ~1) approaches 450ms, against the 300ms an
		// unjittered wait would take.
		wantGapAtLeast = 400 * time.Millisecond
	)

	var (
		mu     sync.Mutex
		starts []time.Time
		draws  atomic.Int64
	)
	s := New(slog.New(slog.DiscardHandler), &fakeLocker{}, Config{Jitter: jitter})
	s.random = func() float64 {
		draws.Add(1)
		return 0.9999999
	}
	if err := s.Register(Job{
		Name:     "poll",
		Interval: interval,
		Run: func(context.Context) error {
			mu.Lock()
			starts = append(starts, time.Now())
			mu.Unlock()
			return nil
		},
	}); err != nil {
		t.Fatalf("Register() error: %v", err)
	}

	began := time.Now()
	stop := start(t, s)
	waitFor(t, "three runs", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(starts) >= 3
	})
	if err := stop(); err != nil {
		t.Errorf("Run() error: %v", err)
	}

	// One draw for the startup wait, then one per gap: by the time the
	// third run begins that is exactly three. A loop that skipped the
	// startup wait would have drawn twice.
	if got := draws.Load(); got < 3 {
		t.Errorf("the jitter was drawn %d times before the third run, want at least 3: the startup wait must be jittered too", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if got := starts[0].Sub(began); got < wantStartupAtLeast {
		t.Errorf("the first run began %v after Run started, want at least %v: the startup wait must be jittered, not skipped", got, wantStartupAtLeast)
	}
	for i := 1; i < len(starts); i++ {
		if gap := starts[i].Sub(starts[i-1]); gap < wantGapAtLeast {
			t.Errorf("run %d began %v after run %d, want at least %v: the wait between runs must be jittered, not the bare interval",
				i, gap, i-1, wantGapAtLeast)
		}
	}
}

func TestCancellationDuringTheStartupDelayStopsTheJobBeforeItRuns(t *testing.T) {
	t.Parallel()

	var runs atomic.Int64
	// The highest draw and a long interval put the first run minutes away,
	// so cancellation lands while the job is still waiting to start.
	s := New(slog.New(slog.DiscardHandler), &fakeLocker{}, Config{})
	s.random = func() float64 { return 0.9 }
	if err := s.Register(Job{
		Name:     "poll",
		Interval: time.Hour,
		Run: func(context.Context) error {
			runs.Add(1)
			return nil
		},
	}); err != nil {
		t.Fatalf("Register() error: %v", err)
	}

	stop := start(t, s)
	if err := stop(); err != nil {
		t.Errorf("Run() error: %v", err)
	}
	if got := runs.Load(); got != 0 {
		t.Errorf("the job ran %d times, want 0: cancellation must end the startup wait", got)
	}
}

func TestCancellationGivesAnInFlightJobItsChanceToFinish(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{})
	var (
		once     sync.Once
		finished atomic.Bool
	)
	locker := &fakeLocker{}
	s := newTestScheduler(locker, Config{ShutdownGrace: 30 * time.Second})
	if err := s.Register(Job{
		Name:     "slow",
		Interval: time.Hour,
		Run: func(ctx context.Context) error {
			once.Do(func() { close(entered) })
			<-ctx.Done()
			// A job that winds down after cancellation rather than
			// abandoning its work mid-way: the grace exists for this.
			time.Sleep(20 * time.Millisecond)
			finished.Store(true)
			return nil
		},
	}); err != nil {
		t.Fatalf("Register() error: %v", err)
	}
	stop := start(t, s)

	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the job never started")
	}
	if err := stop(); err != nil {
		t.Errorf("Run() error = %v, want nil: the job finished inside the grace", err)
	}
	if !finished.Load() {
		t.Error("Run returned before the in-flight job finished")
	}
	if _, released := locker.counts(); released != 1 {
		t.Errorf("locks released = %d, want 1: a cancelled run must still release its lock", released)
	}
}

func TestShutdownGraceIsBounded(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once
	s := newTestScheduler(&fakeLocker{}, Config{ShutdownGrace: 20 * time.Millisecond})
	if err := s.Register(Job{
		Name:     "wedged",
		Interval: time.Hour,
		Run: func(context.Context) error {
			once.Do(func() { close(entered) })
			// Deliberately deaf to cancellation: the grace is what keeps
			// a job like this from holding shutdown open forever.
			<-done
			return nil
		},
	}); err != nil {
		t.Fatalf("Register() error: %v", err)
	}
	stop := start(t, s)

	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the job never started")
	}
	if err := stop(); !errors.Is(err, ErrShutdownTimeout) {
		t.Errorf("Run() with a job past the grace = %v, want ErrShutdownTimeout", err)
	}
	// Let the abandoned goroutine end before the test does.
	close(done)
}

func TestAPanickingJobNeitherStopsItsOwnScheduleNorTheOthers(t *testing.T) {
	t.Parallel()

	var panics, steady atomic.Int64
	locker := &fakeLocker{}
	s := newTestScheduler(locker, Config{})
	if err := s.Register(Job{
		Name:     "boom",
		Interval: time.Millisecond,
		Run: func(context.Context) error {
			panics.Add(1)
			panic("the job exploded")
		},
	}); err != nil {
		t.Fatalf("Register(boom) error: %v", err)
	}
	if err := s.Register(Job{
		Name:     "steady",
		Interval: time.Millisecond,
		Run: func(context.Context) error {
			steady.Add(1)
			return nil
		},
	}); err != nil {
		t.Fatalf("Register(steady) error: %v", err)
	}
	stop := start(t, s)

	waitFor(t, "the panicking job to run again after its panic", func() bool { return panics.Load() >= 3 })
	waitFor(t, "the other job to keep running", func() bool { return steady.Load() >= 3 })
	if err := stop(); err != nil {
		t.Errorf("Run() error = %v, want nil: a panicking job must not fail the scheduler", err)
	}

	taken, released := locker.counts()
	if taken != released {
		t.Errorf("locks taken = %d, released = %d; a panicking run must still release its lock", taken, released)
	}
}

func TestAJobHeldByAnotherInstanceIsSkipped(t *testing.T) {
	t.Parallel()

	var runs, attempts atomic.Int64
	locker := &fakeLocker{grant: func(string) (bool, error) {
		attempts.Add(1)
		return false, nil
	}}
	s := newTestScheduler(locker, Config{})
	if err := s.Register(Job{
		Name:     "poll",
		Interval: time.Millisecond,
		Run: func(context.Context) error {
			runs.Add(1)
			return nil
		},
	}); err != nil {
		t.Fatalf("Register() error: %v", err)
	}
	stop := start(t, s)

	waitFor(t, "the schedule to keep trying for the lock", func() bool { return attempts.Load() >= 3 })
	if err := stop(); err != nil {
		t.Errorf("Run() error: %v", err)
	}
	if got := runs.Load(); got != 0 {
		t.Errorf("the job ran %d times while another instance held its lock, want 0", got)
	}
}

func TestALockFailureIsSurvivedAndTheScheduleContinues(t *testing.T) {
	t.Parallel()

	var attempts, runs atomic.Int64
	locker := &fakeLocker{grant: func(string) (bool, error) {
		// The first two attempts fail the way a database being restarted
		// fails; the schedule must reach the third.
		if attempts.Add(1) <= 2 {
			return false, errLock
		}
		return true, nil
	}}
	s := newTestScheduler(locker, Config{})
	if err := s.Register(Job{
		Name:     "poll",
		Interval: time.Millisecond,
		Run: func(context.Context) error {
			runs.Add(1)
			return nil
		},
	}); err != nil {
		t.Fatalf("Register() error: %v", err)
	}
	stop := start(t, s)

	waitFor(t, "the job to run once the lock came back", func() bool { return runs.Load() >= 1 })
	if err := stop(); err != nil {
		t.Errorf("Run() error: %v", err)
	}
}

func TestAReleaseFailureIsSurvivedAndTheScheduleContinues(t *testing.T) {
	t.Parallel()

	var runs atomic.Int64
	locker := &fakeLocker{releaseErr: errLock}
	s := newTestScheduler(locker, Config{})
	if err := s.Register(Job{
		Name:     "poll",
		Interval: time.Millisecond,
		Run: func(context.Context) error {
			runs.Add(1)
			return nil
		},
	}); err != nil {
		t.Fatalf("Register() error: %v", err)
	}
	stop := start(t, s)

	waitFor(t, "the job to run despite failing releases", func() bool { return runs.Load() >= 3 })
	if err := stop(); err != nil {
		t.Errorf("Run() error: %v", err)
	}
}

func TestAnOverrunningJobSkipsItsNextRunAndNeverQueues(t *testing.T) {
	t.Parallel()

	// The job takes far longer than its interval, so a scheduler that
	// queued ticks would have a backlog of them waiting the moment a run
	// ended, and the next run would begin immediately.
	const (
		interval = time.Millisecond
		duration = 50 * time.Millisecond
	)

	var (
		mu        sync.Mutex
		starts    []time.Time
		inFlight  atomic.Int64
		maxFlight atomic.Int64
	)
	s := newTestScheduler(&fakeLocker{}, Config{})
	if err := s.Register(Job{
		Name:     "slow-poll",
		Interval: interval,
		Run: func(context.Context) error {
			if n := inFlight.Add(1); n > maxFlight.Load() {
				maxFlight.Store(n)
			}
			defer inFlight.Add(-1)

			mu.Lock()
			starts = append(starts, time.Now())
			mu.Unlock()

			time.Sleep(duration)
			return nil
		},
	}); err != nil {
		t.Fatalf("Register() error: %v", err)
	}
	stop := start(t, s)

	waitFor(t, "three runs of the overrunning job", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(starts) >= 3
	})
	if err := stop(); err != nil {
		t.Errorf("Run() error: %v", err)
	}

	if got := maxFlight.Load(); got != 1 {
		t.Errorf("maximum concurrent runs = %d, want 1: a job must never overlap itself", got)
	}

	mu.Lock()
	defer mu.Unlock()
	for i := 1; i < len(starts); i++ {
		if gap := starts[i].Sub(starts[i-1]); gap < duration {
			t.Errorf("run %d began %v after run %d, want at least %v: a missed tick must be dropped, not queued behind the run that missed it",
				i, gap, i-1, duration)
		}
	}
}

func TestJobTimeoutEndsARunAndFreesTheLock(t *testing.T) {
	t.Parallel()

	got := make(chan error, 1)
	locker := &fakeLocker{}
	s := newTestScheduler(locker, Config{})
	if err := s.Register(Job{
		Name:     "wedging",
		Interval: time.Hour,
		Timeout:  20 * time.Millisecond,
		Run: func(ctx context.Context) error {
			<-ctx.Done()
			select {
			case got <- ctx.Err():
			default:
			}
			return ctx.Err()
		},
	}); err != nil {
		t.Fatalf("Register() error: %v", err)
	}
	stop := start(t, s)

	select {
	case err := <-got:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("the job's context ended with %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the job's timeout never fired")
	}
	if err := stop(); err != nil {
		t.Errorf("Run() error: %v", err)
	}

	waitFor(t, "the timed-out run to release its lock", func() bool {
		taken, released := locker.counts()
		return taken == 1 && released == 1
	})
}

func TestRunOnce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		grant    func(string) (bool, error)
		jobErr   error
		wantRan  bool
		wantRuns int64
		wantErr  error
	}{
		{name: "granted, the job runs", wantRan: true, wantRuns: 1},
		{
			name:    "held by another instance, the job is skipped",
			grant:   func(string) (bool, error) { return false, nil },
			wantRan: false,
		},
		{
			name:    "the lock attempt fails, the job is skipped",
			grant:   func(string) (bool, error) { return false, errLock },
			wantErr: errLock,
		},
		{
			name:     "the job fails, and says so",
			jobErr:   errJob,
			wantRan:  true,
			wantRuns: 1,
			wantErr:  errJob,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var runs atomic.Int64
			locker := &fakeLocker{grant: tt.grant}
			s := newTestScheduler(locker, Config{})
			if err := s.Register(Job{
				Name:     "poll",
				Interval: time.Hour,
				Run: func(context.Context) error {
					runs.Add(1)
					return tt.jobErr
				},
			}); err != nil {
				t.Fatalf("Register() error: %v", err)
			}

			ran, err := s.RunOnce(context.Background(), "poll")
			if ran != tt.wantRan {
				t.Errorf("RunOnce() ran = %v, want %v", ran, tt.wantRan)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("RunOnce() error = %v, want %v", err, tt.wantErr)
			}
			if got := runs.Load(); got != tt.wantRuns {
				t.Errorf("job runs = %d, want %d", got, tt.wantRuns)
			}
			if taken, released := locker.counts(); taken != released {
				t.Errorf("locks taken = %d, released = %d", taken, released)
			}
		})
	}
}

func TestRunOnceRejectsAnUnknownJob(t *testing.T) {
	t.Parallel()

	s := newTestScheduler(&fakeLocker{}, Config{})
	ran, err := s.RunOnce(context.Background(), "nobody")
	if ran {
		t.Error("RunOnce() of an unknown job reported that it ran")
	}
	if err == nil || !strings.Contains(err.Error(), "no job named") {
		t.Errorf("RunOnce() of an unknown job = %v, want an error naming the job", err)
	}
}

func TestRunOnceReportsAPanicAsAnError(t *testing.T) {
	t.Parallel()

	s := newTestScheduler(&fakeLocker{}, Config{})
	if err := s.Register(Job{
		Name:     "boom",
		Interval: time.Hour,
		Run:      func(context.Context) error { panic("the job exploded") },
	}); err != nil {
		t.Fatalf("Register() error: %v", err)
	}

	ran, err := s.RunOnce(context.Background(), "boom")
	if !ran {
		t.Error("RunOnce() of a panicking job reported that it did not run")
	}
	var panicked *PanicError
	if !errors.As(err, &panicked) {
		t.Fatalf("RunOnce() error = %v (%T), want a *PanicError", err, err)
	}
	if panicked.Job != "boom" {
		t.Errorf("PanicError.Job = %q, want %q", panicked.Job, "boom")
	}
	if panicked.Value != "the job exploded" {
		t.Errorf("PanicError.Value = %v, want the panic value", panicked.Value)
	}
	if len(panicked.Stack) == 0 {
		t.Error("PanicError.Stack is empty; the stack is the only way to find the bug")
	}
	if !strings.Contains(panicked.Error(), "the job exploded") {
		t.Errorf("PanicError.Error() = %q, want it to carry the panic value", panicked.Error())
	}
}

func TestOutcomesAreLoggedStructurally(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		grant     func(string) (bool, error)
		run       func(context.Context) error
		wantMsg   string
		wantLevel string
		wantError bool
		wantStack bool
	}{
		{
			name:      "a successful run",
			run:       func(context.Context) error { return nil },
			wantMsg:   "job completed",
			wantLevel: "INFO",
		},
		{
			name:      "a failed run",
			run:       func(context.Context) error { return errJob },
			wantMsg:   "job failed",
			wantLevel: "ERROR",
			wantError: true,
		},
		{
			name:      "a panicking run",
			run:       func(context.Context) error { panic("the job exploded") },
			wantMsg:   "job panicked",
			wantLevel: "ERROR",
			wantError: true,
			wantStack: true,
		},
		{
			// Debug, deliberately: every instance but one loses this race
			// on every tick, so at Info it would drown the records above.
			name:      "a run another instance is already doing",
			grant:     func(string) (bool, error) { return false, nil },
			run:       func(context.Context) error { return nil },
			wantMsg:   "job skipped: another instance holds its lock",
			wantLevel: "DEBUG",
		},
		{
			name:      "a lock that could not be taken",
			grant:     func(string) (bool, error) { return false, errLock },
			run:       func(context.Context) error { return nil },
			wantMsg:   "taking the job lock failed",
			wantLevel: "ERROR",
			wantError: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var out strings.Builder
			s := New(logging.New(&out, slog.LevelDebug, config.EnvProd), &fakeLocker{grant: tt.grant}, Config{})
			if err := s.Register(Job{Name: "zero-sum", Interval: time.Hour, Run: tt.run}); err != nil {
				t.Fatalf("Register() error: %v", err)
			}
			if _, err := s.RunOnce(context.Background(), "zero-sum"); err != nil && !tt.wantError {
				t.Fatalf("RunOnce() error: %v", err)
			}

			record := findLogRecord(t, out.String(), tt.wantMsg)
			if got := record["job"]; got != "zero-sum" {
				t.Errorf("log record %q carries job = %v, want the job's name as its own attribute", tt.wantMsg, got)
			}
			if got := record["level"]; got != tt.wantLevel {
				t.Errorf("log record %q is at level %v, want %v", tt.wantMsg, got, tt.wantLevel)
			}
			if _, ok := record["error"]; ok != tt.wantError {
				t.Errorf("log record %q carries an error attribute = %v, want %v", tt.wantMsg, ok, tt.wantError)
			}
			if _, ok := record["stack"]; ok != tt.wantStack {
				t.Errorf("log record %q carries a stack attribute = %v, want %v", tt.wantMsg, ok, tt.wantStack)
			}
		})
	}
}

func TestAReleaseFailureIsLoggedAgainstItsJob(t *testing.T) {
	t.Parallel()

	var out strings.Builder
	s := New(logging.New(&out, slog.LevelDebug, config.EnvProd), &fakeLocker{releaseErr: errLock}, Config{})
	if err := s.Register(Job{
		Name:     "zero-sum",
		Interval: time.Hour,
		Run:      func(context.Context) error { return nil },
	}); err != nil {
		t.Fatalf("Register() error: %v", err)
	}
	if _, err := s.RunOnce(context.Background(), "zero-sum"); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	record := findLogRecord(t, out.String(), "releasing the job lock failed")
	if got := record["job"]; got != "zero-sum" {
		t.Errorf("the release failure names job = %v, want %q", got, "zero-sum")
	}
	if _, ok := record["error"]; !ok {
		t.Error("the release failure carries no error attribute")
	}
}

// findLogRecord returns the first JSON log record in out whose message is msg,
// failing the test when there is none.
func findLogRecord(t *testing.T, out, msg string) map[string]any {
	t.Helper()
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line is not JSON: %v\n%s", err, line)
		}
		if record["msg"] == msg {
			return record
		}
	}
	t.Fatalf("no log record with msg %q in:\n%s", msg, out)
	return nil
}

func TestWait(t *testing.T) {
	t.Parallel()

	cancelled := func() context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}

	tests := []struct {
		name string
		ctx  func() context.Context
		d    time.Duration
		want bool
	}{
		{name: "a zero wait does not wait", ctx: context.Background, d: 0, want: true},
		{name: "a negative wait does not wait", ctx: context.Background, d: -time.Second, want: true},
		{name: "a short wait elapses", ctx: context.Background, d: time.Millisecond, want: true},
		{name: "an ended context does not wait", ctx: cancelled, d: time.Hour, want: false},
		{name: "an ended context skips even a zero wait", ctx: cancelled, d: 0, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := wait(tt.ctx(), tt.d); got != tt.want {
				t.Errorf("wait(%v) = %v, want %v", tt.d, got, tt.want)
			}
		})
	}
}

func TestWaitEndsWhenTheContextDoes(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(10*time.Millisecond, cancel)
	if wait(ctx, time.Hour) {
		t.Error("wait() reported that an hour elapsed; cancellation must end it")
	}
}
