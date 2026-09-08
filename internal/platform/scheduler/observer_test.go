package scheduler

// What an Observer is told, and what it cannot do (ADR-0007).
//
// The seam exists so that one implementation covers every job. That only holds
// if every path through runOnce reports - including the two that are not the
// happy one, which are precisely the paths a hand-instrumented job would have
// forgotten.

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingObserver keeps every attempt it was told about.
type recordingObserver struct {
	mu       sync.Mutex
	attempts []Attempt
	// panicOn, when set, makes JobAttempted panic for that job.
	panicOn string
}

func (r *recordingObserver) JobAttempted(_ context.Context, attempt Attempt) {
	r.mu.Lock()
	r.attempts = append(r.attempts, attempt)
	r.mu.Unlock()
	if r.panicOn != "" && attempt.Job == r.panicOn {
		panic("the observer is broken")
	}
}

func (r *recordingObserver) seen() []Attempt {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Attempt(nil), r.attempts...)
}

// TestEveryOutcomeIsReported. Three outcomes, three paths through runOnce, and
// the two unhappy ones are the reason this is a scheduler-level seam rather
// than a line in each job: a job cannot report a run it never got to make.
func TestEveryOutcomeIsReported(t *testing.T) {
	t.Parallel()
	errLocking := errors.New("the lock could not be taken")
	errJob := errors.New("the job failed")

	for _, tc := range []struct {
		name    string
		grant   func(string) (bool, error)
		run     func(context.Context) error
		want    Outcome
		wantErr error
		panics  bool
	}{
		{name: "ran", run: func(context.Context) error { return nil }, want: Ran},
		{
			name: "failed", run: func(context.Context) error { return errJob },
			want: Ran, wantErr: errJob,
		},
		{
			name: "panicked", run: func(context.Context) error { panic("boom") },
			want: Ran, panics: true,
		},
		{
			name:  "skipped",
			grant: func(string) (bool, error) { return false, nil },
			run:   func(context.Context) error { return nil },
			want:  Skipped,
		},
		{
			name:    "lock failed",
			grant:   func(string) (bool, error) { return false, errLocking },
			run:     func(context.Context) error { return nil },
			want:    LockFailed,
			wantErr: errLocking,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			observer := &recordingObserver{}
			s := New(slog.New(slog.DiscardHandler), &fakeLocker{grant: tc.grant},
				Config{Observer: observer})
			if err := s.Register(Job{Name: "sweep", Interval: time.Minute, Run: tc.run}); err != nil {
				t.Fatalf("registering: %v", err)
			}
			//nolint:errcheck // the outcome under test is what the observer saw.
			_, _ = s.RunOnce(context.Background(), "sweep")

			seen := observer.seen()
			if len(seen) != 1 {
				t.Fatalf("the observer was told about %d attempts, want exactly 1: %+v", len(seen), seen)
			}
			got := seen[0]
			if got.Job != "sweep" {
				t.Errorf("job = %q, want %q", got.Job, "sweep")
			}
			if got.Outcome != tc.want {
				t.Errorf("outcome = %q, want %q", got.Outcome, tc.want)
			}
			if tc.wantErr != nil && !errors.Is(got.Err, tc.wantErr) {
				t.Errorf("err = %v, want %v", got.Err, tc.wantErr)
			}
			if tc.panics != got.Panicked {
				t.Errorf("panicked = %v, want %v", got.Panicked, tc.panics)
			}
			if tc.panics && got.Err == nil {
				t.Error("a panic was reported with no error to read")
			}
		})
	}
}

// TestOnlyARunIsTimed. A skip took no time worth reporting, and a duration
// attached to one would be the wait for a lock masquerading as work done -
// which is exactly the number somebody would later put on a latency chart.
func TestOnlyARunIsTimed(t *testing.T) {
	t.Parallel()
	observer := &recordingObserver{}
	s := New(slog.New(slog.DiscardHandler),
		&fakeLocker{grant: func(string) (bool, error) { return false, nil }},
		Config{Observer: observer})
	if err := s.Register(Job{Name: "sweep", Interval: time.Minute,
		Run: func(context.Context) error { return nil }}); err != nil {
		t.Fatalf("registering: %v", err)
	}
	//nolint:errcheck // the outcome under test is what the observer saw.
	_, _ = s.RunOnce(context.Background(), "sweep")

	seen := observer.seen()
	if len(seen) != 1 {
		t.Fatalf("attempts = %d, want 1", len(seen))
	}
	if seen[0].Took != 0 {
		t.Errorf("a skipped attempt was timed at %v, want zero", seen[0].Took)
	}
}

// TestARunIsTimed, and the duration is the job's rather than the scheduler's.
func TestARunIsTimed(t *testing.T) {
	t.Parallel()
	observer := &recordingObserver{}
	s := New(slog.New(slog.DiscardHandler), &fakeLocker{}, Config{Observer: observer})
	const slept = 20 * time.Millisecond
	if err := s.Register(Job{Name: "sweep", Interval: time.Minute,
		Run: func(context.Context) error { time.Sleep(slept); return nil }}); err != nil {
		t.Fatalf("registering: %v", err)
	}
	//nolint:errcheck // the outcome under test is what the observer saw.
	_, _ = s.RunOnce(context.Background(), "sweep")

	seen := observer.seen()
	if len(seen) != 1 {
		t.Fatalf("attempts = %d, want 1", len(seen))
	}
	if seen[0].Took < slept {
		t.Errorf("took = %v, want at least the %v the job slept", seen[0].Took, slept)
	}
}

// TestNoObserverChangesNothing. Every other test in this repository runs with
// Observer nil, so this is the configuration that has to keep working.
func TestNoObserverChangesNothing(t *testing.T) {
	t.Parallel()
	s := New(slog.New(slog.DiscardHandler), &fakeLocker{}, Config{})
	ran := false
	if err := s.Register(Job{Name: "sweep", Interval: time.Minute,
		Run: func(context.Context) error { ran = true; return nil }}); err != nil {
		t.Fatalf("registering: %v", err)
	}
	did, err := s.RunOnce(context.Background(), "sweep")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !did || !ran {
		t.Error("the job did not run with no observer configured")
	}
}

// TestAnObserverThatPanicsDoesNotStopTheJob.
//
// This is the one that matters. runOnce is called from loop, which has no
// recover of its own, so an unguarded panic here would unwind the goroutine
// that schedules this job - and that job would then never run again for the
// lifetime of the process, silently, with the settlement sweep as likely a
// casualty as anything else. Telemetry that can stop a money job is worse
// than no telemetry.
func TestAnObserverThatPanicsDoesNotStopTheJob(t *testing.T) {
	t.Parallel()
	var logged strings.Builder
	observer := &recordingObserver{panicOn: "sweep"}
	s := New(slog.New(slog.NewTextHandler(&logged, nil)), &fakeLocker{},
		Config{Observer: observer})
	runs := 0
	if err := s.Register(Job{Name: "sweep", Interval: time.Minute,
		Run: func(context.Context) error { runs++; return nil }}); err != nil {
		t.Fatalf("registering: %v", err)
	}

	for i := range 3 {
		ran, err := s.RunOnce(context.Background(), "sweep")
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if !ran {
			t.Fatalf("run %d did not happen", i)
		}
	}
	if runs != 3 {
		t.Errorf("the job ran %d times, want 3", runs)
	}
	if !strings.Contains(logged.String(), "observer panicked") {
		t.Errorf("the panic was swallowed without a word:\n%s", logged.String())
	}
}
