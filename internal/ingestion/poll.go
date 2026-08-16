package ingestion

// The poll loop: the other half of T013, driving fetch.go on a schedule.
// One cycle walks every active source, fetches each conditionally with its
// stored validators, records what arrived through the provenance write path
// (I-2), and writes each source's outcome back onto its row. The loop's
// posture continues the fetcher's: one request per source per interval,
// spaced within a cycle and jittered between cycles, and a source that asks
// to be left alone (Retry-After) is left alone.
//
// Both halves of that posture are enforced by persisted state, because the
// api deployment is replicated and every replica runs this loop: a cycle
// only fetches sources whose last_polled_at is older than the interval, so
// N replicas' interleaved schedules still produce one fetch per source per
// interval, and a Retry-After is written to next_poll_not_before, so a
// source's ask binds the fleet and not just the process that saw the 429.
// The advisory lock underneath prevents overlap only - two cycles running
// at once - not repetition; repetition is the row state's job.
//
// A per-source failure is recorded and stepped over, never allowed to stop
// the cycle: one broken feed must not silence every other source.

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// DefaultPollSpacing is the pause between two sources within one
	// cycle: enough to keep a cycle from reading as a burst against the
	// operator's own infrastructure, short enough that a handful of
	// municipal feeds is walked in seconds.
	DefaultPollSpacing = 2 * time.Second
	// pollJitterFraction spreads each between-cycles wait by up to this
	// share of the interval either way, so N replicas restarted together
	// do not synchronise the fleet into polling in lockstep.
	pollJitterFraction = 0.1
	// pollLockName names the fleet-wide advisory lock under which a cycle
	// runs. hashtext() maps it onto the integer key space Postgres locks
	// on; the name is hashed, not stored, so it only ever needs to stay
	// equal to itself.
	pollLockName = "apivo.poll"
	// pollUnlockTimeout bounds releasing the advisory lock. serve() waits
	// for the poll loop before closing the pool, so an unlock left to a
	// dead-but-undetected connection would hold the whole process's
	// shutdown past its grace period; after this long the connection is
	// destroyed instead, and Postgres frees the session's locks with it.
	pollUnlockTimeout = 5 * time.Second
)

// PollConfig describes how the loop paces itself and how it fetches.
type PollConfig struct {
	// Interval is the wait between cycles, before jitter, and the promise
	// the cycle enforces against the database: a source polled within the
	// interval (less the jitter's early swing - see pollGate) is not
	// fetched again, whichever replica's cycle asks. The composition root
	// only starts the loop with a positive interval; POLL_INTERVAL=0 means
	// the loop is never constructed at all.
	Interval time.Duration
	// Spacing is the pause between two sources within a cycle. Zero means
	// DefaultPollSpacing.
	Spacing time.Duration
	// Fetch configures the conditional GETs. The zero value is the
	// fetcher's own defaults.
	Fetch FetchConfig
}

// Poller walks the active sources on an interval. Construct with NewPoller;
// Run drives it, PollOnce is one locked cycle for callers - and tests -
// that bring their own schedule.
type Poller struct {
	log     *slog.Logger
	pool    *pgxpool.Pool
	sources *SourceStore
	store   *Store
	cfg     PollConfig

	// random feeds the jitter; a field so tests can pin it.
	random func() float64
}

// NewPoller builds the poll loop on the process's connection pool - the
// same pool the HTTP server uses, per the composition root.
func NewPoller(log *slog.Logger, pool *pgxpool.Pool, cfg PollConfig) *Poller {
	if cfg.Spacing <= 0 {
		cfg.Spacing = DefaultPollSpacing
	}
	return &Poller{
		log:     log,
		pool:    pool,
		sources: NewSourceStore(pool),
		store:   NewStore(pool),
		cfg:     cfg,
		//nolint:gosec // G404: jitter spreads schedules; it guards nothing.
		random: rand.Float64,
	}
}

// Run polls until ctx ends: one cycle, then the configured interval with
// jitter, again. The first cycle starts immediately - a fresh deployment
// should not sit idle for a full interval before its first poll.
func (p *Poller) Run(ctx context.Context) {
	for {
		if _, err := p.PollOnce(ctx); err != nil && ctx.Err() == nil {
			// A failed cycle is this process's problem, never fatal: the
			// next interval retries from scratch.
			p.log.ErrorContext(ctx, "poll cycle failed", "error", err)
		}
		if err := sleepContext(ctx, jitteredInterval(p.cfg.Interval, p.random)); err != nil {
			return
		}
	}
}

// PollOnce runs one cycle under the fleet-wide advisory lock, reporting
// whether this process ran a cycle at all. The lock exists because the api
// deployment is replicated - deploy/k8s/api-hpa.yaml runs minReplicas: 2 -
// and every replica runs this loop: pg_try_advisory_lock keeps two cycles
// from running at once, so no source is mid-fetch from two processes. It
// prevents overlap and nothing else - the rate promise (one fetch per
// source per interval, Retry-After honoured fleet-wide) is enforced by the
// persisted state ListActiveSources filters on, which makes a cycle that
// wins the lock when nothing is due a natural no-op. The lock is
// session-scoped, so it is taken on a dedicated connection held for
// exactly one cycle and released with it.
func (p *Poller) PollOnce(ctx context.Context) (bool, error) {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("ingestion: poll: acquire lock connection: %w", err)
	}
	defer conn.Release()

	var held bool
	if err := conn.QueryRow(ctx,
		`select pg_try_advisory_lock(hashtext($1))`, pollLockName).Scan(&held); err != nil {
		return false, fmt.Errorf("ingestion: poll: take advisory lock: %w", err)
	}
	if !held {
		p.log.InfoContext(ctx, "poll cycle skipped: another replica holds the poll lock")
		return false, nil
	}
	defer func() {
		// Unlocked even when ctx has ended - a shutdown must not strand
		// the fleet lock on a pooled session, where it would outlive this
		// cycle and block every future one for the process's lifetime -
		// but never waited on for longer than pollUnlockTimeout: serve()
		// holds shutdown open for this loop, and an unresponsive
		// connection must not hold it past the grace period. If the
		// unlock fails or times out, the connection is destroyed instead
		// - the Close below runs under the same expiring deadline, so on
		// timeout it abandons the socket rather than waiting for a clean
		// goodbye - and Postgres frees the session's advisory locks when
		// the backend dies.
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pollUnlockTimeout)
		defer cancel()
		var released bool
		err := conn.QueryRow(unlockCtx,
			`select pg_advisory_unlock(hashtext($1))`, pollLockName).Scan(&released)
		if err != nil || !released {
			p.log.ErrorContext(ctx, "releasing the poll lock failed; destroying its connection instead", "error", err)
			_ = conn.Conn().Close(unlockCtx)
		}
	}()

	return true, p.cycle(ctx)
}

// cycle walks the active sources once. The list itself is the gate: it
// omits sources polled within pollGate of the interval and sources still
// inside a Retry-After window, so a cycle beginning when nothing is due -
// another replica's, typically - is a natural no-op. Only infrastructure
// failures - the source list being unreadable, the context ending -
// surface as its error; a single source failing is recorded on that source
// and stepped over.
func (p *Poller) cycle(ctx context.Context) error {
	sources, err := p.sources.ListActiveSources(ctx, pollGate(p.cfg.Interval))
	if err != nil {
		return err
	}
	polled := false
	for _, src := range sources {
		// The spacing sleeps between requests, so it precedes every fetch
		// but the cycle's first.
		if polled {
			if err := sleepContext(ctx, p.cfg.Spacing); err != nil {
				return err
			}
		}
		polled = true
		p.pollSource(ctx, src)
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}

// pollSource fetches one source, records what arrived, and writes the
// outcome back. It returns nothing: every failure it meets belongs to this
// source alone, and the cycle continues regardless.
func (p *Poller) pollSource(ctx context.Context, src PolledSource) {
	result, err := p.cfg.Fetch.Fetch(ctx, src.URL, src.Validators)

	outcome := PollOutcome{Validators: refreshedValidators(src.Validators, result)}
	switch {
	case err != nil:
		// A failed exchange refreshes nothing: the stored validators still
		// describe the last document actually seen. A Retry-After the
		// source sent is written with the outcome, deferring the source
		// for every replica; recording the outcome without one clears any
		// standing deferral, because this attempt has spent it.
		outcome.Validators = src.Validators
		outcome.Error = err.Error()
		outcome.RetryAfter = result.RetryAfter
		p.log.WarnContext(ctx, "polling source failed",
			"source_id", src.ID, "url", redactedFeedURL(src.URL), "error", err)
	case result.NotModified:
		// The outcome conditional GET exists to produce, not an error: the
		// counters stay zero, the validators are refreshed, and the empty
		// Error clears any earlier failure.
	default:
		outcome.Retrieved, outcome.Duplicates, err = p.recordItems(ctx, src, result.Items)
		if err != nil {
			// The counters stand for what was stored before the failure:
			// an honest partial record, never a success state for writes
			// that did not happen. The validators stay the stored ones for
			// the same reason: advancing them would let the next cycle's
			// 304 confirm a document whose items were never fully written,
			// hiding the missing ones for as long as the document stands.
			// Refetching unconditionally is safe - RecordRetrieval's
			// content_hash dedupe absorbs the items that did commit.
			outcome.Validators = src.Validators
			outcome.Error = err.Error()
			p.log.WarnContext(ctx, "recording retrieved items failed",
				"source_id", src.ID, "url", redactedFeedURL(src.URL), "error", err)
		}
	}

	if err := p.sources.RecordPollOutcome(ctx, src.ID, outcome); err != nil {
		p.log.ErrorContext(ctx, "recording poll outcome failed",
			"source_id", src.ID, "url", redactedFeedURL(src.URL), "error", err)
	}
}

// recordItems writes each retrieved item through the provenance write path,
// counting new evidence and FR-014 duplicates separately. An item the feed
// rendered unusable (no link, no text) is logged and skipped - it is not
// evidence and not this source failing. A database failure stops this
// source's items and reports the counts stored so far.
func (p *Poller) recordItems(ctx context.Context, src PolledSource, items []NormalizedItem) (retrieved, duplicates int, err error) {
	for _, item := range items {
		if verr := item.Validate(); verr != nil {
			p.log.WarnContext(ctx, "skipping unusable feed item",
				"source_id", src.ID, "url", redactedFeedURL(src.URL), "error", verr)
			continue
		}
		res, werr := p.store.RecordRetrieval(ctx, src.ID, item)
		if werr != nil {
			return retrieved, duplicates, werr
		}
		if res.Duplicate {
			duplicates++
		} else {
			retrieved++
		}
	}
	return retrieved, duplicates, nil
}

// refreshedValidators is what a successful exchange leaves stored: the
// response's own tokens, keeping the stored one wherever the response
// stated none (Result's contract: empty means the response carried none,
// and the caller keeps whatever it already had).
func refreshedValidators(stored Validators, result Result) Validators {
	next := Validators{ETag: result.ETag, LastModified: result.LastModified}
	if next.ETag == "" {
		next.ETag = stored.ETag
	}
	if next.LastModified == "" {
		next.LastModified = stored.LastModified
	}
	return next
}

// pollGate is how long a source must have gone unpolled before a cycle
// fetches it again: the interval, less the jitter's full early swing. The
// gate cannot be the interval itself - Run deliberately lets a cycle begin
// up to pollJitterFraction early, and a gate at the full interval would
// turn every early cycle into a fleet-wide skip, stretching the real
// cadence toward twice the interval. At interval minus the maximum early
// jitter, the earliest cycle Run can produce still finds its sources due,
// and the fetch rate stays bounded by one per source per jittered interval.
func pollGate(interval time.Duration) time.Duration {
	return time.Duration((1 - pollJitterFraction) * float64(interval))
}

// jitteredInterval is one between-cycles wait: the interval, moved up to
// pollJitterFraction of itself either way. random supplies [0, 1).
func jitteredInterval(interval time.Duration, random func() float64) time.Duration {
	if interval <= 0 {
		return 0
	}
	offset := (random()*2 - 1) * pollJitterFraction * float64(interval)
	return interval + time.Duration(offset)
}
