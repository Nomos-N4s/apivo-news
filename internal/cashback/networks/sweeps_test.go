// The tests for sweeps.go: what a pair of sweeps refuses to be built from,
// what its two jobs are called, and what one run of each actually does to
// the account it polls.
//
// The naming cases are unit tests because a lock name is a pure function of
// the account, and the run cases are against the real schema because what a
// run does is move a cursor - which is the one thing a fake could not
// disagree with this file about and still be wrong in the same direction.

package networks_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/platform/scheduler"
)

// sweepTestLocker hands out every lock it is asked for and remembers the
// names, so a case can assert which job a scheduler actually ran.
type sweepTestLocker struct{ taken []string }

func (l *sweepTestLocker) TryLock(_ context.Context, name string) (scheduler.Lock, bool, error) {
	l.taken = append(l.taken, name)
	return sweepTestLock{}, true, nil
}

type sweepTestLock struct{}

func (sweepTestLock) Release(context.Context) error { return nil }

// sweepTestLog captures what a run logged, as JSON so a case asserts on
// named attributes rather than on the shape of a sentence.
type sweepTestLog struct {
	buf bytes.Buffer
}

func (l *sweepTestLog) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(&l.buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// records decodes every line logged so far.
func (l *sweepTestLog) records(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(l.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("a log line was not JSON: %v (%q)", err, line)
		}
		out = append(out, record)
	}
	return out
}

// only asserts that exactly one line was logged, at the given level, and
// returns it.
func (l *sweepTestLog) only(t *testing.T, level string) map[string]any {
	t.Helper()
	records := l.records(t)
	if len(records) != 1 {
		t.Fatalf("the run logged %d line(s), want 1: %s", len(records), l.buf.String())
	}
	if records[0]["level"] != level {
		t.Fatalf("the run logged at %v, want %s: %s", records[0]["level"], level, l.buf.String())
	}
	return records[0]
}

// TestNewSweepsRefusesWhatCannotRun keeps the refusal at wiring. A job
// registered around a nil poller panics inside the scheduler - recovered and
// logged, once every interval, for the life of the process - and an adapter
// that names no network fails the same way once per tick of two jobs.
func TestNewSweepsRefusesWhatCannotRun(t *testing.T) {
	t.Parallel()

	account := pollerTestAccount(t)
	adapter := pollerTestNetwork(account, pollerTestNothing)
	poller, err := networks.NewPoller(&pollerTestDB{})
	if err != nil {
		t.Fatalf("NewPoller(): %v", err)
	}

	if _, err := networks.NewSweeps(slog.New(slog.DiscardHandler), nil, adapter); !errors.Is(err, networks.ErrNoSweepPoller) {
		t.Errorf("NewSweeps(nil poller) = %v, want one wrapping ErrNoSweepPoller", err)
	}
	if _, err := networks.NewSweeps(slog.New(slog.DiscardHandler), poller, nil); !errors.Is(err, networks.ErrNoSweepPoller) {
		t.Errorf("NewSweeps(nil adapter) = %v, want one wrapping ErrNoSweepPoller", err)
	}

	// The wiring check ValidateNetwork exists to make, made where it is
	// cheap. An adapter with no declared limits would otherwise read an
	// empty window forever, reporting success and storing nothing.
	noLimits := pollerTestNetwork(account, pollerTestNothing)
	noLimits.limits = networks.Limits{}
	if _, err := networks.NewSweeps(slog.New(slog.DiscardHandler), poller, noLimits); !errors.Is(err, networks.ErrInvalidLimits) {
		t.Errorf("NewSweeps(adapter with no limits) = %v, want one wrapping ErrInvalidLimits", err)
	}

	if _, err := networks.NewSweeps(slog.New(slog.DiscardHandler), poller, adapter); err != nil {
		t.Errorf("NewSweeps() refused a usable pair: %v", err)
	}
}

// TestSweepJobNamesNameTheAccount holds the property a shared lock name
// would silently break. The names are what the fleet-wide locks are taken
// on, so two names that were equal would make two independent walks take
// turns: two accounts at one network would poll half as often as they were
// configured to, and an account's re-read would queue behind its own forward
// poll. Neither shows up as an error anywhere.
func TestSweepJobNamesNameTheAccount(t *testing.T) {
	t.Parallel()

	first := pollerTestAccount(t)
	second, err := networks.NewPublisherAccount(first.ID(), first.Network(), "publisher-2")
	if err != nil {
		t.Fatalf("NewPublisherAccount(): %v", err)
	}

	names := map[string]string{
		"the first account's forward sweep":              networks.ForwardJobName(first),
		"the first account's trailing sweep":             networks.TrailingJobName(first),
		"a second account at the same network, forward":  networks.ForwardJobName(second),
		"a second account at the same network, trailing": networks.TrailingJobName(second),
	}
	seen := make(map[string]string, len(names))
	for what, name := range names {
		if other, taken := seen[name]; taken {
			t.Errorf("%s and %s are both named %q; they would take turns holding one lock", what, other, name)
		}
		seen[name] = what
		if !strings.Contains(name, first.ExternalID()) && !strings.Contains(name, "publisher-2") {
			t.Errorf("%s is named %q, which names no publisher account", what, name)
		}
		if !strings.Contains(name, first.Network().String()) {
			t.Errorf("%s is named %q, which names no network", what, name)
		}
	}
}

// TestSweepsAgainstTheRealSchema registers the pair and runs each job
// through the scheduler that will run it in production, so what is asserted
// is the whole path: the job the scheduler knows by name, the poll it makes,
// the cursor it moves, and the one line it leaves behind.
func TestSweepsAgainstTheRealSchema(t *testing.T) {
	t.Parallel()
	ctx, tx := pollerSchemaConnect(t)
	first, second := pollerSchemaPair(t)

	eachPoll(ctx, t, tx, "each sweep runs under its own name and moves its own cursor", func(t *testing.T, tx pgx.Tx) {
		account := pollerSchemaAccount(ctx, t, tx)
		adapter := pollerTestNetwork(account, pollerTestReports(first, second))
		captured := &sweepTestLog{}
		sweeps, err := networks.NewSweeps(captured.logger(),
			pollerSchemaPoller(t, tx, networks.WithTrailingLag(24*time.Hour)), adapter)
		if err != nil {
			t.Fatalf("NewSweeps(): %v", err)
		}

		locker := &sweepTestLocker{}
		jobs := scheduler.New(slog.New(slog.DiscardHandler), locker, scheduler.Config{})
		if err := sweeps.Register(jobs); err != nil {
			t.Fatalf("Register(): %v", err)
		}

		// The forward sweep, driven the way the scheduler drives it.
		ran, err := jobs.RunOnce(ctx, networks.ForwardJobName(account))
		if err != nil || !ran {
			t.Fatalf("the forward job ran=%t, err=%v", ran, err)
		}
		cursors := cursorsOf(ctx, t, tx, account)
		if !cursors.CursorAt.Valid {
			t.Fatal("the forward sweep left the main cursor unset")
		}
		if cursors.TrailingCursorAt.Valid {
			t.Errorf("the forward sweep moved the trailing cursor to %s", cursors.TrailingCursorAt.Time)
		}
		// A run that stored money-bearing evidence is the one shape worth
		// an Info line.
		record := captured.only(t, "INFO")
		if record["first_reports"] != float64(2) {
			t.Errorf("the line reports first_reports=%v, want 2: %s", record["first_reports"], captured.buf.String())
		}
		if record["job"] != networks.ForwardJobName(account) {
			t.Errorf("the line names job %v, want %q", record["job"], networks.ForwardJobName(account))
		}

		// The trailing sweep, over ground the forward cursor has passed.
		afterForward := cursors.CursorAt.Time
		captured.buf.Reset()
		ran, err = jobs.RunOnce(ctx, networks.TrailingJobName(account))
		if err != nil || !ran {
			t.Fatalf("the trailing job ran=%t, err=%v", ran, err)
		}
		cursors = cursorsOf(ctx, t, tx, account)
		if !cursors.TrailingCursorAt.Valid {
			t.Fatal("the trailing sweep left the trailing cursor unset")
		}
		if !cursors.CursorAt.Time.Equal(afterForward) {
			t.Errorf("the trailing sweep moved the main cursor to %s, want it left at %s", cursors.CursorAt.Time, afterForward)
		}
		// The network re-reported exactly what it had said, so nothing was
		// written and nothing is worth an operator's attention. This is
		// most of every trailing sweep, and at Info it would bury the runs
		// that matter.
		if record := captured.only(t, "DEBUG"); record["unchanged"] != float64(2) {
			t.Errorf("the line reports unchanged=%v, want 2: %s", record["unchanged"], captured.buf.String())
		}

		// Both locks were taken, under the names the jobs were registered
		// with - which is what stops a second instance running either.
		wantLocks := []string{networks.ForwardJobName(account), networks.TrailingJobName(account)}
		if len(locker.taken) != 2 || locker.taken[0] != wantLocks[0] || locker.taken[1] != wantLocks[1] {
			t.Errorf("the scheduler took locks %v, want %v", locker.taken, wantLocks)
		}
	})

	eachPoll(ctx, t, tx, "a sweep with nothing to read says so quietly", func(t *testing.T, tx pgx.Tx) {
		account := pollerSchemaAccount(ctx, t, tx)
		adapter := pollerTestNetwork(account, pollerTestReports(first, second))
		captured := &sweepTestLog{}
		// A clock at the backfill start: no period has elapsed to ask about.
		poller := pollerSchemaPoller(t, tx, networks.WithPollerClock(func() time.Time { return pollerSchemaStart }))
		sweeps, err := networks.NewSweeps(captured.logger(), poller, adapter)
		if err != nil {
			t.Fatalf("NewSweeps(): %v", err)
		}

		if err := sweeps.RunForward(ctx); err != nil {
			t.Fatalf("RunForward(): %v", err)
		}
		if record := captured.only(t, "DEBUG"); record["window"] != nil {
			t.Errorf("a sweep that read nothing named a window: %v", record["window"])
		}
		if len(adapter.windows) != 0 {
			t.Errorf("the network was asked for %v", adapter.windows)
		}
	})

	eachPoll(ctx, t, tx, "a failed run is left for the scheduler to report", func(t *testing.T, tx pgx.Tx) {
		account := pollerSchemaAccount(ctx, t, tx)
		broken := errors.New("the network stopped answering")
		adapter := pollerTestNetwork(account, func(int, networks.QueryWindow) ([]networks.Reported, error) {
			return []networks.Reported{first}, broken
		})
		captured := &sweepTestLog{}
		sweeps, err := networks.NewSweeps(captured.logger(), pollerSchemaPoller(t, tx), adapter)
		if err != nil {
			t.Fatalf("NewSweeps(): %v", err)
		}

		if err := sweeps.RunForward(ctx); !errors.Is(err, broken) {
			t.Fatalf("RunForward() = %v, want one wrapping the network's failure", err)
		}
		// The scheduler logs every failed run with the job's name already.
		// A second line saying the same thing at the same moment is how a
		// log stops being read.
		if lines := captured.records(t); len(lines) != 0 {
			t.Errorf("a failed run logged %d line(s) of its own: %s", len(lines), captured.buf.String())
		}
		if cursorsOf(ctx, t, tx, account).CursorAt.Valid {
			t.Error("a failed run moved the cursor")
		}
	})
}
