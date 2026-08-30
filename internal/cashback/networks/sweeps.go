// The two sweeps as scheduled jobs: what they are called, how often they
// run, and what one run reports (T057, FR-031).

package networks

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Nomos-N4s/apivo-news/internal/platform/scheduler"
)

// ErrNoSweepPoller reports sweeps built without the two things a run needs:
// a poller to drive and an adapter to read through. Refused at construction
// rather than at the first tick, because a job registered around a nil
// poller would panic inside the scheduler - recovered and logged, once every
// interval, for the life of the process.
var ErrNoSweepPoller = errors.New("networks: scheduled sweeps need a poller and an adapter")

const (
	// ForwardInterval is how long a period may go unread before the forward
	// sweep asks for it. It is per-instance and it is a LATENCY figure, not
	// a rate: the scheduler's own doc is explicit that its interval bounds
	// neither how often a job runs across a fleet nor how often a network
	// is asked, and what actually stops two instances polling one account
	// at once is the cursor read taking FOR UPDATE.
	//
	// A quarter of an hour is chosen against SC-001, which requires a
	// purchase to reach the member's wallet as pending "no later than 24
	// hours after the network reports it". At this cadence a window has to
	// be missed ninety-five times running before that is at risk, which
	// leaves room for the lock contention a replicated deployment produces
	// by design.
	ForwardInterval = 15 * time.Minute
	// TrailingInterval is the "slower schedule" ADR-0003 puts the re-read
	// on. The sweep walks forward one period per run at a fixed remove, so
	// this decides how lumpy the re-read is rather than how far behind it
	// runs - that is [DefaultTrailingLag]'s job. Four runs a day re-read a
	// day's worth of aged ground a day, which keeps the sweep level with
	// the forward cursor while costing four requests.
	TrailingInterval = 6 * time.Hour
	// sweepTimeout bounds one run of either sweep. A window is fetched a
	// page at a time against a network that may stop answering mid-window,
	// so a run that is still going after this is wedged rather than slow -
	// and a wedged run must free its lock for the next tick rather than
	// stopping the sweep fleet-wide.
	//
	// Being cut short costs nothing but the fetch. The run's context ends,
	// its transaction rolls back whole, and the cursor stays where it was,
	// so the next tick reads the same window again - which is the ordinary
	// half-read-window path (FR-031), not a special case.
	sweepTimeout = 10 * time.Minute
)

// ForwardJobName is the forward sweep's name for one publisher account: its
// name in the scheduler's logs, and the name of its fleet-wide lock.
//
// It names the ACCOUNT rather than the network, because the account is what
// owns the cursors. Two publisher accounts at one network are two
// independent walks over two cursor pairs, and one lock name between them
// would make each wait for the other for no reason - the schema models that
// pair explicitly (migration 0011), so the job name has to as well.
func ForwardJobName(account PublisherAccount) string {
	return "network-poll:" + account.Network().String() + ":" + account.ExternalID()
}

// TrailingJobName is the trailing sweep's name for one publisher account,
// built the same way and for the same reason. It is a name of its own
// because the two sweeps must be able to run at the same time: they move
// different cursors over different periods, and sharing a lock would make
// the re-read wait behind every forward poll.
func TrailingJobName(account PublisherAccount) string {
	return "network-trailing-poll:" + account.Network().String() + ":" + account.ExternalID()
}

// Sweeps is one publisher account's pair of scheduled polls.
//
// The registration lives here rather than in the composition root so that
// each job's identity - its name, its cadence, its timeout - has one owner.
// A second wiring site could otherwise register the same account's poll
// under a name that no longer excludes the first, which would put two
// pollers on one cursor and rely on nothing but the row lock to save them.
type Sweeps struct {
	log     *slog.Logger
	poller  *Poller
	adapter Network
}

// NewSweeps builds the pair over one adapter and the poller that drives it.
//
// It holds the adapter to [ValidateNetwork] here, at wiring, which is where
// that check is meant to be made: an adapter that names no network, speaks
// for no account or declares no limits fails the deployment rather than
// every tick of two jobs for the life of the process. The poller checks the
// same things again on every poll, and that is not redundant - a poller may
// be handed an adapter this constructor never saw.
func NewSweeps(log *slog.Logger, poller *Poller, adapter Network) (*Sweeps, error) {
	if poller == nil || adapter == nil {
		return nil, ErrNoSweepPoller
	}
	if err := ValidateNetwork(adapter); err != nil {
		return nil, err
	}
	return &Sweeps{log: log, poller: poller, adapter: adapter}, nil
}

// Account is the publisher account these sweeps poll, so a caller that
// registered them can name it in a log line without holding it separately.
func (s *Sweeps) Account() PublisherAccount { return s.adapter.Account() }

// Register puts both sweeps on the scheduler. Two jobs, always: an account
// polled forward but never re-read would show every transaction as pending
// forever, because a confirmation only ever arrives as a re-report of a
// window already read (ADR-0003).
func (s *Sweeps) Register(sch *scheduler.Scheduler) error {
	account := s.adapter.Account()
	if err := sch.Register(scheduler.Job{
		Name:     ForwardJobName(account),
		Interval: ForwardInterval,
		Timeout:  sweepTimeout,
		Run:      s.RunForward,
	}); err != nil {
		return err
	}
	return sch.Register(scheduler.Job{
		Name:     TrailingJobName(account),
		Interval: TrailingInterval,
		Timeout:  sweepTimeout,
		Run:      s.RunTrailing,
	})
}

// RunForward is one run of the forward sweep: read the next period nobody
// has read, and advance the main cursor over it.
func (s *Sweeps) RunForward(ctx context.Context) error {
	poll, err := s.poller.PollForward(ctx, s.adapter)
	return s.report(ctx, ForwardJobName(s.adapter.Account()), poll, err)
}

// RunTrailing is one run of the re-read sweep: walk ground the forward
// cursor passed long enough ago that the network has had time to change its
// mind about it, and advance only the trailing cursor.
func (s *Sweeps) RunTrailing(ctx context.Context) error {
	poll, err := s.poller.PollTrailing(ctx, s.adapter)
	return s.report(ctx, TrailingJobName(s.adapter.Account()), poll, err)
}

// report logs what a run did and hands the error back unchanged.
//
// A failure is NOT logged here. The scheduler logs every failed run with the
// job's name already, and a second line saying the same thing at the same
// moment is how a log stops being read - while the error itself already
// names the account, the window and the sentinel it wraps, which is what an
// operator needs (US2 scenario 5). What this adds is only what the scheduler
// cannot see: what a SUCCESSFUL run found.
//
// Only a run that changed what somebody is owed is worth an Info line. A
// poll that found nothing to read, and a re-read that found the network
// still saying exactly what it said before, are both the steady state - the
// second is most of every trailing sweep - and at Info they would bury the
// one run that ever says anything else.
func (s *Sweeps) report(ctx context.Context, job string, poll Poll, err error) error {
	switch {
	case err != nil:
		return err
	case !poll.Ran:
		s.log.DebugContext(ctx, "the sweep had nothing to read", "job", job, "account", s.adapter.Account().String())
	case poll.Outcome.Changed():
		s.log.InfoContext(ctx, "the sweep stored what the network reported",
			"job", job, "account", s.adapter.Account().String(), "window", poll.Window.String(),
			"first_reports", poll.Outcome.FirstReports, "superseded", poll.Outcome.Superseded,
			"unchanged", poll.Outcome.Unchanged, "cursor_at", poll.CursorAdvancedTo)
	default:
		s.log.DebugContext(ctx, "the sweep read a period in which nothing had changed",
			"job", job, "account", s.adapter.Account().String(), "window", poll.Window.String(),
			"unchanged", poll.Outcome.Unchanged, "cursor_at", poll.CursorAdvancedTo)
	}
	return nil
}
