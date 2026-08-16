package ingestion

// The poll loop: the other half of T013, driving fetch.go on a schedule.
// One cycle walks every active source, fetches each conditionally with its
// stored validators, records what arrived through the provenance write path
// (I-2), and writes each source's outcome back onto its row. The loop's
// posture continues the fetcher's: one request per source per interval,
// spaced within a cycle and jittered between cycles, and a source that asks
// to be left alone (Retry-After) is left alone.
//
// A per-source failure is recorded and stepped over, never allowed to stop
// the cycle: one broken feed must not silence every other source.

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
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
)

// PollConfig describes how the loop paces itself and how it fetches.
type PollConfig struct {
	// Interval is the wait between cycles, before jitter. The composition
	// root only starts the loop with a positive interval; POLL_INTERVAL=0
	// means the loop is never constructed at all.
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

	// deferredUntil holds, per source, when a Retry-After the source asked
	// for expires. It defers that source alone - cycles beginning before
	// the moment skip it, everything else polls normally - and it is
	// in-memory deliberately: a restart forgetting a deferral costs one
	// polite request, which the source answers with another Retry-After if
	// it still means it.
	deferredUntil map[uuid.UUID]time.Time

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
		log:           log,
		pool:          pool,
		sources:       NewSourceStore(pool),
		store:         NewStore(pool),
		cfg:           cfg,
		deferredUntil: make(map[uuid.UUID]time.Time),
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
// whether this process actually polled. The lock exists because the api
// deployment is replicated - deploy/k8s/api-hpa.yaml runs minReplicas: 2 -
// and every replica runs this loop: pg_try_advisory_lock makes them one
// poller, with whoever tries second skipping the cycle instead of doubling
// the requests to every source. The lock is session-scoped, so it is taken
// on a dedicated connection held for exactly one cycle and released with
// it.
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
		// cycle and block every future one for the process's lifetime. If
		// the unlock itself fails, the session is destroyed instead:
		// Postgres releases its advisory locks with it.
		unlockCtx := context.WithoutCancel(ctx)
		var released bool
		err := conn.QueryRow(unlockCtx,
			`select pg_advisory_unlock(hashtext($1))`, pollLockName).Scan(&released)
		if err != nil || !released {
			p.log.ErrorContext(ctx, "releasing the poll lock failed; closing its connection instead", "error", err)
			_ = conn.Conn().Close(unlockCtx)
		}
	}()

	return true, p.cycle(ctx)
}

// cycle walks the active sources once. Only infrastructure failures - the
// source list being unreadable, the context ending - surface as its error;
// a single source failing is recorded on that source and stepped over.
func (p *Poller) cycle(ctx context.Context) error {
	start := time.Now()
	sources, err := p.sources.ListActiveSources(ctx)
	if err != nil {
		return err
	}
	polled := false
	for _, src := range sources {
		if until, ok := p.deferredUntil[src.ID]; ok && sourceDeferred(start, until) {
			p.log.InfoContext(ctx, "source deferred by its own Retry-After; skipping this cycle",
				"source_id", src.ID, "until", until)
			continue
		}
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
	// The attempt happened, so any standing deferral is spent; a new
	// Retry-After below starts a fresh one.
	delete(p.deferredUntil, src.ID)

	outcome := PollOutcome{Validators: refreshedValidators(src.Validators, result)}
	switch {
	case err != nil:
		// A failed exchange refreshes nothing: the stored validators still
		// describe the last document actually seen.
		outcome.Validators = src.Validators
		outcome.Error = err.Error()
		if result.RetryAfter > 0 {
			p.deferredUntil[src.ID] = time.Now().Add(result.RetryAfter)
		}
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

// sourceDeferred reports whether a cycle beginning at cycleStart must leave
// a source deferred until the given moment alone. Only cycles that begin
// strictly before the deferral ends skip it; a cycle beginning exactly then
// polls, because the source's wait has been served in full.
func sourceDeferred(cycleStart, until time.Time) bool {
	return cycleStart.Before(until)
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
