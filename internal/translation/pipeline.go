package translation

// The translation pipeline: the loop that turns retrieved items into paid,
// recorded translations (T018). It connects the pieces the earlier rounds
// built - the Translator seam, the frozen prompts, the Caps/Ledger/Writer
// cost controls - and adds the one thing none of them could own alone: the
// order in which the budget is consulted, the provider is paid, and the
// result is recorded.
//
// The posture throughout is FR-006's: the pipeline halts rather than
// overspends. The budget is checked BEFORE every provider call, because
// checking afterwards only reports what has already been spent; a failure
// that cost money still reaches the ledger; and when the month is halted
// or at its cap the cycle stops immediately - the halt itself is latched
// by the database (migration 0005), never announced from here.
//
// The api deployment is replicated and every replica runs this loop, so
// the unit of work is claimed, not assumed: each (item, locale) pair is
// taken under a pg_try_advisory_xact_lock inside the very transaction
// that will write the translation, and a worker that cannot claim a pair
// skips it. Two replicas therefore never pay a provider twice for the
// same pair - the claim guards the money, while the unique index from
// 0005 guards the record.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Nomos-N4s/apivo-news/internal/platform/text"
)

const (
	// DefaultCycleLimit bounds how many items one cycle considers. The
	// bound exists so a fresh deployment pointed at a backlog of items
	// spends at a rate an operator can watch and stop, rather than racing
	// the whole backlog to the monthly cap in one cycle.
	DefaultCycleLimit = 10

	// recordAttempts bounds the ErrRetryable contract's retries: the
	// writer books nothing on a transient failure and the retry is what
	// settles the ledger, so giving up too early is what leaves a billed
	// call off the month. Three attempts covers a deadlock victim or a
	// dropped statement without holding the claim open for long.
	recordAttempts = 3
	// recordBackoff is the wait between those attempts.
	recordBackoff = 200 * time.Millisecond
)

// PipelineDB is the slice of database access the pipeline needs: the
// Writer's transactional DB plus multi-row queries for the work list.
// *pgxpool.Pool satisfies it in production; tests pass a transaction.
type PipelineDB interface {
	DB
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// PipelineConfig describes one pipeline: its cadence, its budget, and what
// it translates into.
type PipelineConfig struct {
	// Interval is the wait between cycles. The composition root only
	// starts the pipeline with a positive interval; TRANSLATION_INTERVAL=0
	// means it is never constructed at all.
	Interval time.Duration

	// Limit bounds the items one cycle considers, newest first. Zero
	// means DefaultCycleLimit.
	Limit int

	// ReaderLocales are the languages readers read in; every item is
	// translated into each of them except its own source language. The
	// alpha passes AlphaReaderLocales.
	ReaderLocales []string

	// PromptVersion names the registered prompt new translations are
	// requested with. Empty means CurrentPromptVersion.
	PromptVersion string

	// Caps is the FR-006 budget: the per-article ceiling and the monthly
	// cap. Required - a missing budget is not an unlimited one.
	Caps Caps
}

// Pipeline drives the translation loop. Construct with NewPipeline; Run
// drives it on the configured interval, RunOnce is one cycle for callers -
// and tests - that bring their own schedule.
type Pipeline struct {
	log        *slog.Logger
	db         PipelineDB
	translator Translator
	cfg        PipelineConfig
}

// NewPipeline validates the configuration and returns a ready pipeline.
// Configuration is checked once, here, rather than surfacing as a failed
// cycle every interval: a pipeline that cannot enforce its budget or name
// its prompt must not start.
func NewPipeline(log *slog.Logger, db PipelineDB, translator Translator, cfg PipelineConfig) (*Pipeline, error) {
	if err := cfg.Caps.Validate(); err != nil {
		return nil, fmt.Errorf("translation: building the pipeline: %w", err)
	}
	if cfg.PromptVersion == "" {
		cfg.PromptVersion = CurrentPromptVersion
	}
	if _, err := PromptByVersion(cfg.PromptVersion); err != nil {
		return nil, fmt.Errorf("translation: building the pipeline: %w", err)
	}
	if len(cfg.ReaderLocales) == 0 {
		return nil, errors.New("translation: building the pipeline: no reader locales to translate into")
	}
	if cfg.Interval <= 0 {
		return nil, fmt.Errorf("translation: building the pipeline: interval must be positive, got %s; a zero interval means the pipeline is disabled and never constructed", cfg.Interval)
	}
	if cfg.Limit <= 0 {
		cfg.Limit = DefaultCycleLimit
	}
	return &Pipeline{log: log, db: db, translator: translator, cfg: cfg}, nil
}

// Run cycles until ctx ends: one cycle, then the configured interval,
// again. The first cycle starts immediately - a fresh deployment should
// not sit on a translatable backlog for a full interval. No jitter and no
// fleet lock, unlike the poll loop: the unit that must not run twice is
// the (item, locale) pair, and the per-pair claim already guards that, so
// two replicas cycling together simply share the work.
func (p *Pipeline) Run(ctx context.Context) {
	for {
		if err := p.RunOnce(ctx); err != nil && ctx.Err() == nil {
			// A failed cycle is this process's problem, never fatal: the
			// next interval retries from scratch.
			p.log.ErrorContext(ctx, "translation cycle failed", "error", err)
		}
		if err := sleepContext(ctx, p.cfg.Interval); err != nil {
			return
		}
	}
}

// RunOnce runs one cycle: check the budget, list the eligible work, and
// translate pair by pair until the list, the budget or the context runs
// out. Only infrastructure failures surface as its error; a single item
// failing is logged and stepped over, because one unusable item must not
// stop every other translation.
func (p *Pipeline) RunOnce(ctx context.Context) error {
	// The pre-flight read (Ledger.ThisMonth's contract): a halted or
	// capped month means NO provider call, and finding that out costs one
	// SELECT rather than one paid translation. Re-checked per pair below,
	// because this cycle's own spend - or another replica's - can cross
	// the cap mid-cycle.
	month, err := NewLedger(p.db).ThisMonth(ctx)
	if err != nil {
		return err
	}
	if p.budgetStop(ctx, month) {
		return nil
	}

	items, err := p.eligibleItems(ctx)
	if err != nil {
		return err
	}
	for _, item := range items {
		for _, locale := range TargetLocales(item.SourceLanguage, p.cfg.ReaderLocales) {
			if err := ctx.Err(); err != nil {
				return err
			}
			stop, err := p.translatePair(ctx, item, locale)
			switch {
			case err == nil:
			case itemFailure(err):
				// This pair is the problem, not the pipeline: step over it
				// and keep translating. The pair stays eligible, so a
				// transient bad answer is retried next cycle.
				p.log.WarnContext(ctx, "translating item failed",
					"source_item_id", item.ID, "target_locale", locale, "error", err)
			case providerFailure(err):
				// The provider or its configuration is the problem, so the
				// next pair would meet exactly the same answer; ending the
				// cycle is what "slow down" means here. The items stay
				// eligible for the next interval.
				p.log.WarnContext(ctx, "ending the translation cycle: the failure is provider-wide, not this item's",
					"source_item_id", item.ID, "target_locale", locale, "error", err)
				return nil
			default:
				// Infrastructure: the database is the problem, and the
				// cycle cannot promise anything more until the next one.
				return err
			}
			if stop {
				return nil
			}
		}
	}
	return nil
}

// workItem is one retrieved item as the pipeline sees it: what to
// translate and the language it is in.
type workItem struct {
	ID             uuid.UUID
	Title          string
	RawBody        string
	SourceLanguage string
}

// eligibleItems lists the items this cycle considers: retrieved items from
// active sources, newest first, still lacking at least one reader-locale
// translation. The per-locale decision is TargetLocales' plus the
// recheck-under-claim in translatePair; this query only keeps the cycle
// from re-reading items with nothing left to do.
//
// Items without a title are excluded here rather than skipped in Go: a
// translation is a headline and an extract, so an item the feed gave no
// title can never produce one - and, being permanently ineligible, it
// would otherwise occupy a slot of the cycle's bound forever.
func (p *Pipeline) eligibleItems(ctx context.Context) ([]workItem, error) {
	rows, err := p.db.Query(ctx,
		`select si.id, si.original_title, si.raw_body, s.language_code
		   from source_item si
		   join source s on s.id = si.source_id
		  where s.active
		    and si.original_title is not null
		    and btrim(si.original_title) <> ''
		    and exists (
		        select 1
		          from unnest($1::text[]) as reader(code)
		         where reader.code <> s.language_code
		           and not exists (
		               select 1 from translation t
		                where t.source_item_id = si.id
		                  and t.target_locale = reader.code))
		  order by si.retrieved_at desc, si.id
		  limit $2`,
		p.cfg.ReaderLocales, p.cfg.Limit)
	if err != nil {
		return nil, fmt.Errorf("translation: listing eligible items: %w", err)
	}
	defer rows.Close()

	var items []workItem
	for rows.Next() {
		var (
			id   string
			item workItem
		)
		if err := rows.Scan(&id, &item.Title, &item.RawBody, &item.SourceLanguage); err != nil {
			return nil, fmt.Errorf("translation: listing eligible items: scan: %w", err)
		}
		item.ID, err = uuid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("translation: listing eligible items: database returned malformed id %q: %w", id, err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("translation: listing eligible items: %w", err)
	}
	return items, nil
}

// translatePair translates one item into one locale: claim, re-check,
// budget, provider, record - in that order, inside one transaction. stop
// reports that the month is done (halted, or at its cap) and the cycle
// must make no further provider calls.
func (p *Pipeline) translatePair(ctx context.Context, item workItem, locale string) (stop bool, err error) {
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("translation: claiming %s into %q: begin: %w", item.ID, locale, err)
	}
	// A no-op after a commit below; the guarantee that every earlier
	// return leaves nothing behind - including the claim, which is
	// transaction-scoped and dies with it.
	defer func() { _ = tx.Rollback(ctx) }()

	// The claim: pair-scoped, inside the transaction that will write the
	// translation, so it cannot outlive the write and cannot be held
	// without it. try, not wait - a pair someone else holds is being paid
	// for right now, and waiting our turn to pay again is the opposite of
	// the point.
	var claimed bool
	if err := tx.QueryRow(ctx,
		`select pg_try_advisory_xact_lock(hashtext($1::text || $2::text))`,
		item.ID.String(), locale).Scan(&claimed); err != nil {
		return false, fmt.Errorf("translation: claiming %s into %q: %w", item.ID, locale, err)
	}
	if !claimed {
		return false, nil
	}

	// Re-check under the claim. The work list was read before this claim
	// existed, so another worker may have translated the pair in between:
	// its claim released on commit, ours succeeded, and only the row says
	// the money was already spent. Without this read the provider would
	// be paid a second time for an answer the unique index then refuses.
	var alreadyTranslated bool
	if err := tx.QueryRow(ctx,
		`select exists (select 1 from translation where source_item_id = $1 and target_locale = $2)`,
		item.ID.String(), locale).Scan(&alreadyTranslated); err != nil {
		return false, fmt.Errorf("translation: re-checking %s into %q under the claim: %w", item.ID, locale, err)
	}
	if alreadyTranslated {
		return false, nil
	}

	// The budget, re-read per pair: this cycle's earlier spend, or a
	// replica's, may have crossed the cap since the cycle's pre-flight.
	month, err := NewLedger(tx).ThisMonth(ctx)
	if err != nil {
		return false, err
	}
	if p.budgetStop(ctx, month) {
		return true, nil
	}

	// The provider is sent the title and a BOUNDED extract of the body,
	// never the body itself: what we publish is an extract beside a link
	// (FR-004), so nothing longer ever needs to leave the system - and
	// input tokens are billed like output ones (FR-006).
	extract := text.DeriveExtract(item.RawBody)
	if strings.TrimSpace(extract) == "" {
		// A body that reduces to no prose (markup only, say) cannot be
		// translated into an extract. Logged and left: the item ages out
		// of the newest-first window on its own.
		p.log.WarnContext(ctx, "skipping item whose body yields no extract",
			"source_item_id", item.ID, "target_locale", locale)
		return false, nil
	}
	result, terr := p.translator.Translate(ctx, Request{
		SourceTitle:    item.Title,
		SourceText:     extract,
		SourceLanguage: item.SourceLanguage,
		TargetLanguage: locale,
		PromptVersion:  p.cfg.PromptVersion,
	})
	if terr != nil {
		return p.settleFailedCall(ctx, tx, terr)
	}

	writer := NewWriter(tx, p.cfg.Caps)
	recorded, rerr := p.record(ctx, writer, Record{SourceItemID: item.ID, TargetLocale: locale, Result: result})
	switch {
	case rerr == nil:
		if err := tx.Commit(ctx); err != nil {
			return false, p.uncommittedSpend(fmt.Errorf("translation: committing %s into %q: %w", item.ID, locale, err), result.Spend)
		}
		p.log.InfoContext(ctx, "translated",
			"source_item_id", item.ID, "target_locale", locale,
			"translation_id", recorded.TranslationID, "model", result.Model,
			"prompt_version", result.PromptVersion, "cost_microusd", result.CostMicroUSD,
			"unmetered_attempts", result.UnmeteredAttempts,
			"month_to_date_microusd", recorded.MonthToDateMicroUSD)
	case errors.Is(rerr, ErrAlreadyTranslated), errors.Is(rerr, ErrOverCeiling):
		// Both are refusals the writer has already settled: the spend is
		// on the ledger, the translation is not, and the refusal and its
		// booking commit together. ErrAlreadyTranslated should be
		// unreachable behind the claim and the re-check, but if it ever
		// happens the honest record of the double payment must land.
		if err := tx.Commit(ctx); err != nil {
			return false, p.uncommittedSpend(errors.Join(rerr, fmt.Errorf("translation: committing the refusal's spend: %w", err)), result.Spend)
		}
		p.log.WarnContext(ctx, "translation refused; its cost is on the ledger",
			"source_item_id", item.ID, "target_locale", locale, "error", rerr,
			"cost_microusd", result.CostMicroUSD,
			"month_to_date_microusd", recorded.MonthToDateMicroUSD)
	default:
		// Transient failures exhausted their retries, or something the
		// writer could not classify: nothing committed, so the deferred
		// rollback stands and the money is not yet on the ledger.
		return false, p.uncommittedSpend(rerr, result.Spend)
	}
	return recorded.Halted(), nil
}

// settleFailedCall books what a failed provider call still cost. A failure
// that cost nothing books nothing; either way the Translate error is
// returned for the cycle to classify, with the booking's own failure - a
// billed call the ledger does not yet show - joined on when it happens.
func (p *Pipeline) settleFailedCall(ctx context.Context, tx pgx.Tx, terr error) (stop bool, err error) {
	var spent *SpendError
	if !errors.As(terr, &spent) {
		return false, terr
	}
	recorded, rerr := NewWriter(tx, p.cfg.Caps).RecordFailedCall(ctx, spent.Spend)
	if rerr != nil {
		return false, errors.Join(terr, p.uncommittedSpend(rerr, spent.Spend))
	}
	if cerr := tx.Commit(ctx); cerr != nil {
		return false, errors.Join(terr, p.uncommittedSpend(fmt.Errorf("translation: committing a failed call's spend: %w", cerr), spent.Spend))
	}
	return recorded.Halted(), terr
}

// uncommittedSpend wraps a failure after which money the provider billed
// is not yet on the ledger. The pair stays eligible and the next cycle
// pays the provider again, so the ledger can drift low by this amount -
// exactly what FR-006 exists to prevent, which is why the amount is named
// in the error an operator will read.
func (p *Pipeline) uncommittedSpend(err error, spend Spend) error {
	if spend.IsZero() {
		return err
	}
	return fmt.Errorf("%w: %d micro-USD (and %d unmetered attempt(s)) were billed but are NOT on the ledger; the month may read low by that amount", err, spend.CostMicroUSD, spend.UnmeteredAttempts)
}

// record drives the writer through its ErrRetryable contract: a transient
// failure books nothing and the retry is what settles the ledger, exactly
// once - so giving up on the first one is what strands a billed call off
// the month.
func (p *Pipeline) record(ctx context.Context, writer *Writer, rec Record) (Recorded, error) {
	var (
		out Recorded
		err error
	)
	for attempt := 1; attempt <= recordAttempts; attempt++ {
		out, err = writer.Record(ctx, rec)
		if !errors.Is(err, ErrRetryable) {
			return out, err
		}
		if attempt < recordAttempts {
			if serr := sleepContext(ctx, recordBackoff); serr != nil {
				return out, err
			}
		}
	}
	return out, err
}

// budgetStop reports whether the month can pay for another call, and logs
// the stop when it cannot. Callers return immediately on true, so the line
// is logged once per cycle, not once per pair - and the halt itself is
// never written here: latching it is the database's, through the writer's
// settle (migration 0005).
func (p *Pipeline) budgetStop(ctx context.Context, month Month) bool {
	switch {
	case month.Halted():
		p.log.InfoContext(ctx, "translation is halted for the month: the cap was reached; no provider call until next month",
			"month", month.Month.Format("2006-01"), "halted_at", month.HaltedAt,
			"spent_microusd", month.SpentMicroUSD, "unmetered_attempts", month.UnmeteredAttempts)
		return true
	case p.cfg.Caps.Reached(month.SpentMicroUSD):
		// At the cap but not yet latched: the next recorded spend will
		// latch the halt; this cycle's job is only to add no more.
		p.log.InfoContext(ctx, "translation month is at its cap: stopping the cycle before any further provider call",
			"month", month.Month.Format("2006-01"),
			"spent_microusd", month.SpentMicroUSD, "monthly_cap_microusd", p.cfg.Caps.MonthlyMicroUSD)
		return true
	}
	return false
}

// itemFailure recognises a failure that belongs to one item: the request
// or the answer was unusable, or this one call ran out of time. The next
// item is unaffected, so the cycle continues.
func itemFailure(err error) bool {
	return errors.Is(err, ErrInvalidRequest) ||
		errors.Is(err, ErrInvalidResponse) ||
		errors.Is(err, ErrTimeout)
}

// providerFailure recognises a failure every further call this cycle
// would share: rejected credentials, an exhausted rate limit, a host that
// is down. The adapter has already spent its retry budget on it, so the
// cycle ends rather than paying that budget once per remaining item.
func providerFailure(err error) bool {
	return errors.Is(err, ErrAuth) ||
		errors.Is(err, ErrRateLimited) ||
		errors.Is(err, ErrUnavailable)
}

// sleepContext waits for d, or until the context ends.
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
