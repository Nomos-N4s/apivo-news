package translation_test

// Integration tests for the pipeline cycle, against the real, migrated
// schema: eligibility, the per-pair claim, the budget stops and the
// ledger movement of failures (FR-006). The writer's own integration
// tests cover the recording mechanics; these prove the PIPELINE drives
// them in the right order - budget before provider, claim before both.
//
// Except for the claim test, every test hands the pipeline a transaction
// that is rolled back, exactly like the writer tests: the ledger is keyed
// by calendar month and shared, so a committing test would move a number
// every other test reads. The claim test needs two sessions, so its seeds
// commit - with unique URLs and bodies, because source_item is immutable
// (I-3) and the rows stay. CI runs against a fresh database every time.

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Nomos-N4s/apivo-news/internal/translation"
)

// rowQuerier is what the seed helpers need; the pool and a transaction
// both satisfy it, so the claim test can seed committed rows through the
// same helpers the rolled-back tests use.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// seedPipelineSource writes one source in the given language.
func seedPipelineSource(t *testing.T, db rowQuerier, lang string, active bool) string {
	t.Helper()
	suffix := uuid.NewString()
	var id string
	if err := db.QueryRow(context.Background(),
		`insert into source (name, url, language_code, jurisdiction, licence_terms, active)
		 values ($1, $2, $3, 'GR', 'Extract and link permitted per feed terms', $4)
		 returning id`,
		"Pipeline Test Feed "+suffix, "https://pipeline.example.test/feed/"+suffix, lang, active).Scan(&id); err != nil {
		t.Fatalf("seeding source: %v", err)
	}
	return id
}

// seedPipelineItem writes one retrieved item. title is a pointer because
// the feed may genuinely have provided none, and that case is part of the
// eligibility contract.
func seedPipelineItem(t *testing.T, db rowQuerier, sourceID string, title *string, body string, retrievedAt time.Time) uuid.UUID {
	t.Helper()
	var id string
	if err := db.QueryRow(context.Background(),
		`insert into source_item (source_id, source_url, original_title, raw_body, retrieved_at)
		 values ($1, $2, $3, $4, $5)
		 returning id`,
		sourceID, "https://pipeline.example.test/articles/"+uuid.NewString(), title, body, retrievedAt).Scan(&id); err != nil {
		t.Fatalf("seeding source_item: %v", err)
	}
	parsed, err := uuid.Parse(id)
	if err != nil {
		t.Fatalf("parsing item id: %v", err)
	}
	return parsed
}

// seedTranslation records an existing translation of the item, as an
// earlier cycle would have left it. The 0005 trigger moves the in-tx
// ledger with it, which is the real shape of the data.
func seedTranslation(t *testing.T, db rowQuerier, itemID uuid.UUID, locale string) {
	t.Helper()
	var id string
	if err := db.QueryRow(context.Background(),
		`insert into translation (source_item_id, target_locale, model, prompt_version, headline, extract, cost_microusd)
		 values ($1, $2, 'seed-model', 'v1', 'Vorhandene Überschrift', 'Vorhandener Auszug.', 100)
		 returning id`,
		itemID.String(), locale).Scan(&id); err != nil {
		t.Fatalf("seeding existing translation: %v", err)
	}
}

// hasTranslation reports whether the item has a translation into the
// locale on this connection's view of the data.
func hasTranslation(t *testing.T, db rowQuerier, itemID uuid.UUID, locale string) bool {
	t.Helper()
	var exists bool
	if err := db.QueryRow(context.Background(),
		`select exists (select 1 from translation where source_item_id = $1 and target_locale = $2)`,
		itemID.String(), locale).Scan(&exists); err != nil {
		t.Fatalf("checking for a translation: %v", err)
	}
	return exists
}

// pipelineResult is a provider answer the budget has no objection to.
func pipelineResult(costMicroUSD int64) translation.Result {
	return translation.Result{
		Headline:      "Übersetzte Überschrift",
		Extract:       "Übersetzter Auszug mit genug Text für einen Anriss.",
		Model:         "fake-model-1",
		PromptVersion: translation.CurrentPromptVersion,
		Spend:         translation.Spend{CostMicroUSD: costMicroUSD},
	}
}

// scriptedTranslator answers by source title: a scripted Result, a
// scripted error, or - for anything unscripted, such as items other tests
// committed - a plain invalid-response failure that costs nothing and is
// stepped over. It records every request, so tests assert on exactly who
// was called. No network anywhere.
type scriptedTranslator struct {
	mu      sync.Mutex
	calls   []translation.Request
	results map[string]translation.Result
	errs    map[string]error
}

func (s *scriptedTranslator) Translate(_ context.Context, req translation.Request) (translation.Result, error) {
	s.mu.Lock()
	s.calls = append(s.calls, req)
	s.mu.Unlock()
	if err, ok := s.errs[req.SourceTitle]; ok {
		return translation.Result{}, err
	}
	if res, ok := s.results[req.SourceTitle]; ok {
		return res, nil
	}
	return translation.Result{}, fmt.Errorf("scripted translator: unscripted item %q: %w", req.SourceTitle, translation.ErrInvalidResponse)
}

// requestsFor returns the recorded requests carrying this source title.
func (s *scriptedTranslator) requestsFor(title string) []translation.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []translation.Request
	for _, req := range s.calls {
		if req.SourceTitle == title {
			out = append(out, req)
		}
	}
	return out
}

// newTestPipeline builds a pipeline on the given connection with a large
// cycle bound: the shared database accumulates committed items from the
// claim test, and a bound that cut our own seeds out of the newest-first
// window would make these tests assert on somebody else's data.
func newTestPipeline(t *testing.T, db translation.PipelineDB, translator translation.Translator, readers []string, caps translation.Caps) *translation.Pipeline {
	t.Helper()
	p, err := translation.NewPipeline(slog.New(slog.DiscardHandler), db, translator, translation.PipelineConfig{
		Interval:      time.Minute,
		Limit:         500,
		ReaderLocales: readers,
		Caps:          caps,
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

// lockedMonth takes the month row's lock for the life of the transaction
// and returns the ledger as of now: with the lock held, every delta a test
// computes is exact even while other packages commit spend.
func lockedMonth(t *testing.T, tx pgx.Tx) translation.Month {
	t.Helper()
	ctx := context.Background()
	if _, err := translation.NewLedger(tx).RecordUnbilledSpend(ctx, translation.Spend{}); err != nil {
		t.Fatalf("locking the month row: %v", err)
	}
	month, err := translation.NewLedger(tx).ThisMonth(ctx)
	if err != nil {
		t.Fatalf("reading the month under lock: %v", err)
	}
	return month
}

// TestEligibilitySelectsExactlyTheUntranslatedForeignPairs asserts the
// work-selection contract: an item is translated into each reader locale
// except its own language, only while untranslated, only from active
// sources, and only when the feed gave it a title and its body yields an
// extract at all.
func TestEligibilitySelectsExactlyTheUntranslatedForeignPairs(t *testing.T) {
	t.Parallel()
	tx := ledgerTx(t)
	ctx := context.Background()

	elGreek := "Ελληνικό επιλέξιμο άρθρο " + uuid.NewString()
	enBoth := "English item for both readers " + uuid.NewString()
	deDone := "Bereits übersetzter Artikel " + uuid.NewString()
	elPaused := "Άρθρο από ανενεργή πηγή " + uuid.NewString()
	elMarkup := "Άρθρο χωρίς πεζό κείμενο " + uuid.NewString()

	elSource := seedPipelineSource(t, tx, "el", true)
	enSource := seedPipelineSource(t, tx, "en", true)
	deSource := seedPipelineSource(t, tx, "de", true)
	pausedSource := seedPipelineSource(t, tx, "el", false)

	now := time.Now()
	greekItem := seedPipelineItem(t, tx, elSource, &elGreek, "Σώμα με αρκετό κείμενο για ένα απόσπασμα.", now)
	englishItem := seedPipelineItem(t, tx, enSource, &enBoth, "A body with enough text for an extract.", now)
	doneItem := seedPipelineItem(t, tx, deSource, &deDone, "Ein Text, der bereits übersetzt wurde.", now)
	seedTranslation(t, tx, doneItem, "el")
	seedPipelineItem(t, tx, pausedSource, &elPaused, "Κείμενο από πηγή σε παύση.", now)
	seedPipelineItem(t, tx, elSource, nil, "Σώμα χωρίς τίτλο από την πηγή.", now)
	markupItem := seedPipelineItem(t, tx, elSource, &elMarkup, "<script>var onlyCode = true;</script>", now)

	fake := &scriptedTranslator{results: map[string]translation.Result{
		elGreek: pipelineResult(1_000),
		enBoth:  pipelineResult(1_000),
	}}
	month := lockedMonth(t, tx)
	caps := translation.Caps{PerArticleMicroUSD: 20_000, MonthlyMicroUSD: month.SpentMicroUSD + 10_000_000}

	pipeline := newTestPipeline(t, tx, fake, []string{"el", "de"}, caps)
	if err := pipeline.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce() = %v, want nil", err)
	}

	// The Greek item serves the German readers only: its own language
	// needs no translation.
	greekCalls := fake.requestsFor(elGreek)
	if len(greekCalls) != 1 || greekCalls[0].TargetLanguage != "de" {
		t.Errorf("Greek item was requested as %+v, want exactly one request into \"de\"", greekCalls)
	}
	if len(greekCalls) == 1 {
		if greekCalls[0].SourceLanguage != "el" {
			t.Errorf("Greek item's request carries source language %q, want \"el\"", greekCalls[0].SourceLanguage)
		}
		// The provider gets a derived extract, never the raw body: here
		// the body is already plain prose within the bound, so the
		// extract is exactly it.
		if greekCalls[0].SourceText != "Σώμα με αρκετό κείμενο για ένα απόσπασμα." {
			t.Errorf("Greek item's request carries text %q, want the derived extract", greekCalls[0].SourceText)
		}
		if greekCalls[0].PromptVersion != translation.CurrentPromptVersion {
			t.Errorf("request carries prompt version %q, want %q", greekCalls[0].PromptVersion, translation.CurrentPromptVersion)
		}
	}

	// The English item serves both reader languages.
	englishCalls := fake.requestsFor(enBoth)
	locales := make([]string, 0, len(englishCalls))
	for _, req := range englishCalls {
		locales = append(locales, req.TargetLanguage)
	}
	if len(englishCalls) != 2 || locales[0] != "el" || locales[1] != "de" {
		t.Errorf("English item was requested into %v, want [el de]", locales)
	}

	// And the exclusions: already translated, paused source, no title,
	// no prose - none of them reaches a provider.
	for _, excluded := range []string{deDone, elPaused, elMarkup} {
		if calls := fake.requestsFor(excluded); len(calls) != 0 {
			t.Errorf("item %q reached the provider %d time(s), want none", excluded, len(calls))
		}
	}

	// What was translated is on record, with its lineage.
	if !hasTranslation(t, tx, greekItem, "de") {
		t.Error("the Greek item's German translation is not on record")
	}
	if !hasTranslation(t, tx, englishItem, "el") || !hasTranslation(t, tx, englishItem, "de") {
		t.Error("the English item's translations are not both on record")
	}
	if hasTranslation(t, tx, markupItem, "de") {
		t.Error("an item with no derivable extract was recorded as translated")
	}
}

// TestAHaltedOrCappedMonthStopsTheCycleBeforeAnyProviderCall asserts the
// FR-006 posture: the budget is consulted before money moves, so a month
// that is halted - or merely at its cap, with the latch not yet taken -
// buys nothing at all.
func TestAHaltedOrCappedMonthStopsTheCycleBeforeAnyProviderCall(t *testing.T) {
	t.Parallel()

	upsertMonth := func(t *testing.T, tx pgx.Tx, spent int64, halted bool) {
		t.Helper()
		haltedAt := "null"
		if halted {
			haltedAt = "now()"
		}
		if _, err := tx.Exec(context.Background(), fmt.Sprintf(
			`insert into translation_spend (month, spent_microusd, unmetered_attempts, halted_at)
			 values (date_trunc('month', now() at time zone 'utc')::date, $1, 0, %s)
			 on conflict (month) do update
			    set spent_microusd = excluded.spent_microusd,
			        halted_at = excluded.halted_at`, haltedAt), spent); err != nil {
			t.Fatalf("writing the month row: %v", err)
		}
	}

	tests := []struct {
		name   string
		spent  int64
		halted bool
	}{
		{"halted month", 500, true},
		{"month at its cap, latch not yet taken", 1_000, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tx := ledgerTx(t)
			title := "Δεν πρέπει να μεταφραστεί " + uuid.NewString()
			source := seedPipelineSource(t, tx, "el", true)
			seedPipelineItem(t, tx, source, &title, "Κείμενο που δεν πρέπει να σταλεί πουθενά.", time.Now())
			upsertMonth(t, tx, tt.spent, tt.halted)

			fake := &scriptedTranslator{}
			caps := translation.Caps{PerArticleMicroUSD: 1_000, MonthlyMicroUSD: 1_000}
			pipeline := newTestPipeline(t, tx, fake, []string{"de"}, caps)
			if err := pipeline.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce() = %v, want nil", err)
			}
			if len(fake.calls) != 0 {
				t.Errorf("the provider was called %d time(s), want none: the stop must come before any call", len(fake.calls))
			}
		})
	}
}

// TestAnOverCeilingResultIsRefusedAndTheCycleContinues asserts two halves
// of one posture: the expensive result is never recorded as a translation
// while its cost still lands on the ledger, and the refusal is that
// PAIR's problem - the cycle goes on to translate the next item.
func TestAnOverCeilingResultIsRefusedAndTheCycleContinues(t *testing.T) {
	t.Parallel()
	tx := ledgerTx(t)
	ctx := context.Background()

	expensive := "Πανάκριβο άρθρο " + uuid.NewString()
	affordable := "Προσιτό άρθρο " + uuid.NewString()
	source := seedPipelineSource(t, tx, "el", true)
	// The expensive item is newer, so the cycle meets it first and the
	// affordable one proves the continuation.
	expensiveItem := seedPipelineItem(t, tx, source, &expensive, "Κείμενο του πανάκριβου άρθρου.", time.Now())
	affordableItem := seedPipelineItem(t, tx, source, &affordable, "Κείμενο του προσιτού άρθρου.", time.Now().Add(-time.Minute))

	fake := &scriptedTranslator{results: map[string]translation.Result{
		expensive:  pipelineResult(50_000),
		affordable: pipelineResult(1_000),
	}}
	before := lockedMonth(t, tx)
	caps := translation.Caps{PerArticleMicroUSD: 20_000, MonthlyMicroUSD: before.SpentMicroUSD + 10_000_000}

	pipeline := newTestPipeline(t, tx, fake, []string{"de"}, caps)
	if err := pipeline.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce() = %v, want nil", err)
	}

	if hasTranslation(t, tx, expensiveItem, "de") {
		t.Error("an over-ceiling result was recorded as a translation")
	}
	if !hasTranslation(t, tx, affordableItem, "de") {
		t.Error("the affordable item was not translated: an over-ceiling refusal must not end the cycle")
	}
	month, err := translation.NewLedger(tx).ThisMonth(ctx)
	if err != nil {
		t.Fatalf("reading the month back: %v", err)
	}
	if got := month.SpentMicroUSD - before.SpentMicroUSD; got != 51_000 {
		t.Errorf("month moved by %d micro-USD, want 51000: the refused call was billed too", got)
	}
}

// TestAFailedCallsSpendStillReachesTheLedger asserts the pipeline's half
// of the SpendError contract: a provider call that failed - and was billed
// anyway - moves the month exactly like a success, and stores nothing.
func TestAFailedCallsSpendStillReachesTheLedger(t *testing.T) {
	t.Parallel()
	tx := ledgerTx(t)
	ctx := context.Background()

	title := "Άρθρο που απέτυχε επί πληρωμή " + uuid.NewString()
	source := seedPipelineSource(t, tx, "el", true)
	item := seedPipelineItem(t, tx, source, &title, "Κείμενο που θα χρεωθεί χωρίς αποτέλεσμα.", time.Now())

	fake := &scriptedTranslator{errs: map[string]error{
		title: &translation.SpendError{
			Spend: translation.Spend{CostMicroUSD: 700, UnmeteredAttempts: 1},
			Err:   translation.ErrInvalidResponse,
		},
	}}
	before := lockedMonth(t, tx)
	caps := translation.Caps{PerArticleMicroUSD: 20_000, MonthlyMicroUSD: before.SpentMicroUSD + 10_000_000}

	pipeline := newTestPipeline(t, tx, fake, []string{"de"}, caps)
	if err := pipeline.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce() = %v, want nil: a failed item is stepped over, not fatal", err)
	}

	if hasTranslation(t, tx, item, "de") {
		t.Error("a failed call left a translation on record")
	}
	month, err := translation.NewLedger(tx).ThisMonth(ctx)
	if err != nil {
		t.Fatalf("reading the month back: %v", err)
	}
	if got := month.SpentMicroUSD - before.SpentMicroUSD; got != 700 {
		t.Errorf("month moved by %d micro-USD, want 700: the failure was billed and must be on the ledger", got)
	}
	if got := month.UnmeteredAttempts - before.UnmeteredAttempts; got != 1 {
		t.Errorf("month's unmetered attempts moved by %d, want 1", got)
	}
}

// blockingTranslator holds its one scripted item open until released, so
// a test can pin the moment a cycle holds that pair's claim mid-provider
// call. Everything else fails unscripted, free of charge.
type blockingTranslator struct {
	scriptedTranslator
	title   string
	started chan struct{}
	release chan struct{}
	once    sync.Once
	result  translation.Result
}

func (b *blockingTranslator) Translate(ctx context.Context, req translation.Request) (translation.Result, error) {
	if req.SourceTitle != b.title {
		return b.scriptedTranslator.Translate(ctx, req)
	}
	b.mu.Lock()
	b.calls = append(b.calls, req)
	b.mu.Unlock()
	b.once.Do(func() { close(b.started) })
	select {
	case <-b.release:
	case <-ctx.Done():
		return translation.Result{}, ctx.Err()
	}
	return b.result, nil
}

// TestTwoConcurrentCyclesPayForAPairOnce asserts the claim: while one
// replica's cycle is mid-provider-call on a pair - claim held inside the
// transaction that will write the translation - a second replica's cycle
// walks past that pair without paying for it.
//
// Two real sessions are required, so the seeds commit (unique URLs and
// bodies; source_item is immutable, so they stay). Both cycles run in
// transactions that roll back, so neither the translation nor any ledger
// movement outlives the test.
//
// Deliberately NOT t.Parallel: the committed item is eligible work for
// every pipeline in this package, and an advisory xact lock survives to
// the end of the transaction that took it - so a parallel test's cycle
// stepping over the item would hold its claim for that whole test, and
// the first cycle here would skip the very pair the test is about.
// Running serially, the parallel tests are paused while this one owns
// the table.
func TestTwoConcurrentCyclesPayForAPairOnce(t *testing.T) {
	if ledgerTestPool == nil {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the claim")
	}
	ctx := context.Background()

	title := "Άρθρο της διεκδίκησης " + uuid.NewString()
	source := seedPipelineSource(t, ledgerTestPool, "el", true)
	item := seedPipelineItem(t, ledgerTestPool, source, &title, "Κείμενο για τη δοκιμή της διεκδίκησης.", time.Now())

	tx1, err := ledgerTestPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin first session: %v", err)
	}
	t.Cleanup(func() { _ = tx1.Rollback(context.Background()) })
	tx2, err := ledgerTestPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin second session: %v", err)
	}
	t.Cleanup(func() { _ = tx2.Rollback(context.Background()) })

	first := &blockingTranslator{
		title:   title,
		started: make(chan struct{}),
		release: make(chan struct{}),
		result:  pipelineResult(1_000),
	}
	second := &scriptedTranslator{}
	caps := translation.Caps{PerArticleMicroUSD: 20_000, MonthlyMicroUSD: 25_000_000}
	firstCycle := newTestPipeline(t, tx1, first, []string{"de"}, caps)
	secondCycle := newTestPipeline(t, tx2, second, []string{"de"}, caps)

	done := make(chan error, 1)
	go func() { done <- firstCycle.RunOnce(ctx) }()
	select {
	case <-first.started:
		// The first cycle now holds the claim on (item, de) and is inside
		// the provider call - the exact window a second replica meets.
	case <-time.After(60 * time.Second):
		t.Fatal("the first cycle never reached the seeded item")
	}

	if err := secondCycle.RunOnce(ctx); err != nil {
		t.Fatalf("second RunOnce() = %v, want nil", err)
	}
	close(first.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("first RunOnce() = %v, want nil", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the first cycle never finished after release")
	}

	if calls := first.requestsFor(title); len(calls) != 1 {
		t.Errorf("the first cycle paid for the pair %d time(s), want exactly once", len(calls))
	}
	if calls := second.requestsFor(title); len(calls) != 0 {
		t.Errorf("the second cycle paid for a pair the first held the claim on: %d call(s), want none", len(calls))
	}
	if !hasTranslation(t, tx1, item, "de") {
		t.Error("the claimed pair's translation is not on the first session's record")
	}
}
