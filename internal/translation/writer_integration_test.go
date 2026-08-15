package translation_test

// Integration tests for the write path where the budget meets the database
// (FR-006). The Writer is handed a transaction rather than the pool, which
// costs nothing - pgx.Tx opens its own nested transactions - and buys full
// isolation: the ledger is keyed by calendar month, so a committing test
// would move a number every other test and every later run reads.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Nomos-N4s/apivo-news/internal/translation"
)

// seedItem writes a source and one retrieved item to translate.
func seedItem(t *testing.T, tx pgx.Tx) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()

	var sourceID string
	if err := tx.QueryRow(ctx,
		`insert into source (name, url, language_code, jurisdiction, licence_terms)
		 values ($1, $2, 'el', 'GR', 'Extract and link permitted per feed terms') returning id`,
		"Spend Test Feed "+suffix, "https://spend.example.test/feed/"+suffix).Scan(&sourceID); err != nil {
		t.Fatalf("seeding source: %v", err)
	}
	var itemID string
	if err := tx.QueryRow(ctx,
		`insert into source_item (source_id, source_url, raw_body) values ($1, $2, $3) returning id`,
		sourceID, "https://spend.example.test/articles/"+suffix, "σώμα κειμένου "+suffix).Scan(&itemID); err != nil {
		t.Fatalf("seeding source_item: %v", err)
	}
	parsed, err := uuid.Parse(itemID)
	if err != nil {
		t.Fatalf("parsing item id: %v", err)
	}
	return parsed
}

// affordableResult is a translation the budget has no objection to.
func affordableResult(costMicroUSD int64, unmetered int) translation.Result {
	return translation.Result{
		Headline:      "Testüberschrift",
		Extract:       "Testauszug mit genug Text für einen Anriss.",
		Model:         "test-model-1",
		PromptVersion: "prompt-v1",
		Spend:         translation.Spend{CostMicroUSD: costMicroUSD, UnmeteredAttempts: unmetered},
	}
}

// translationCount counts the translations of one item into one locale.
func translationCount(t *testing.T, tx pgx.Tx, itemID uuid.UUID, locale string) int {
	t.Helper()
	var n int
	if err := tx.QueryRow(context.Background(),
		`select count(*) from translation where source_item_id = $1 and target_locale = $2`,
		itemID.String(), locale).Scan(&n); err != nil {
		t.Fatalf("counting translations: %v", err)
	}
	return n
}

// TestTheCeilingRefusesBeforeTheInsert asserts the whole point of keeping
// the ceiling in application code: an over-ceiling result is never written,
// AND its spend still reaches the ledger. A CHECK on cost_microusd would
// have got the first half right and destroyed the second - refusing to
// record money that had already left the account.
func TestTheCeilingRefusesBeforeTheInsert(t *testing.T) {
	t.Parallel()
	tx := ledgerTx(t)
	ctx := context.Background()
	itemID := seedItem(t, tx)

	ledger := translation.NewLedger(tx)
	before, err := ledger.RecordUnbilledSpend(ctx, translation.Spend{})
	if err != nil {
		t.Fatalf("reading the month under lock: %v", err)
	}
	// A cap far above anything this test spends: the halt is the other
	// test's subject, and a month that halted here would say nothing
	// about the ceiling.
	caps := translation.Caps{PerArticleMicroUSD: 20_000, MonthlyMicroUSD: before + 10_000_000}
	writer := translation.NewWriter(tx, caps)

	out, err := writer.Record(ctx, translation.Record{
		SourceItemID: itemID,
		TargetLocale: "de",
		Result:       affordableResult(50_000, 2),
	})
	if !errors.Is(err, translation.ErrOverCeiling) {
		t.Fatalf("Record() error = %v, want ErrOverCeiling", err)
	}
	if out.TranslationID != uuid.Nil {
		t.Errorf("Record() reported translation %s for a result it refused to write", out.TranslationID)
	}
	if n := translationCount(t, tx, itemID, "de"); n != 0 {
		t.Errorf("translations on record = %d, want 0: an over-ceiling result must never be written", n)
	}

	// The money, however, is gone, and the ledger says so.
	if got := out.MonthToDateMicroUSD - before; got != 50_000 {
		t.Errorf("month moved by %d micro-USD, want 50000: the provider billed for the call whatever the price", got)
	}
	month, err := ledger.ThisMonth(ctx)
	if err != nil {
		t.Fatalf("reading the month back: %v", err)
	}
	if month.SpentMicroUSD != out.MonthToDateMicroUSD {
		t.Errorf("ledger holds %d micro-USD, want the %d Record reported", month.SpentMicroUSD, out.MonthToDateMicroUSD)
	}
	if month.Halted() {
		t.Error("the month halted: this test's cap is far above what it spends")
	}

	// And the pipeline is not poisoned: the next item translates normally.
	otherID := seedItem(t, tx)
	next, err := writer.Record(ctx, translation.Record{
		SourceItemID: otherID,
		TargetLocale: "de",
		Result:       affordableResult(1_500, 0),
	})
	if err != nil {
		t.Fatalf("a within-ceiling translation after a refusal: %v", err)
	}
	if next.TranslationID == uuid.Nil {
		t.Error("a within-ceiling translation reported no id")
	}
	if got := next.MonthToDateMicroUSD - out.MonthToDateMicroUSD; got != 1_500 {
		t.Errorf("month moved by %d micro-USD, want 1500 - written by trigger, not by this module", got)
	}
}

// TestASecondRecordOfTheSameItemAndLocaleIsRefusedAndStillCharged asserts
// the re-run case end to end: the database refuses the duplicate, the
// module reports it as a refusal rather than a failure, and the money the
// duplicate call cost is still on the ledger.
func TestASecondRecordOfTheSameItemAndLocaleIsRefusedAndStillCharged(t *testing.T) {
	t.Parallel()
	tx := ledgerTx(t)
	ctx := context.Background()
	itemID := seedItem(t, tx)

	ledger := translation.NewLedger(tx)
	before, err := ledger.RecordUnbilledSpend(ctx, translation.Spend{})
	if err != nil {
		t.Fatalf("reading the month under lock: %v", err)
	}
	writer := translation.NewWriter(tx, translation.Caps{PerArticleMicroUSD: 20_000, MonthlyMicroUSD: before + 10_000_000})

	first, err := writer.Record(ctx, translation.Record{
		SourceItemID: itemID,
		TargetLocale: "de",
		Result:       affordableResult(1_500, 0),
	})
	if err != nil {
		t.Fatalf("first translation: %v", err)
	}

	second, err := writer.Record(ctx, translation.Record{
		SourceItemID: itemID,
		TargetLocale: "de",
		Result:       affordableResult(1_700, 1),
	})
	if !errors.Is(err, translation.ErrAlreadyTranslated) {
		t.Fatalf("Record() error = %v, want ErrAlreadyTranslated", err)
	}
	if second.TranslationID != uuid.Nil {
		t.Errorf("the refused re-run reported translation %s", second.TranslationID)
	}
	if n := translationCount(t, tx, itemID, "de"); n != 1 {
		t.Errorf("translations on record = %d, want 1: a re-run must not buy a second approvable origin", n)
	}
	if got := second.MonthToDateMicroUSD - first.MonthToDateMicroUSD; got != 1_700 {
		t.Errorf("month moved by %d micro-USD, want 1700: the duplicate call was billed too", got)
	}

	// The lower bound travels with it: the duplicate's unpriced attempt is
	// on the month, so the total still says how confident it is.
	month, err := ledger.ThisMonth(ctx)
	if err != nil {
		t.Fatalf("reading the month back: %v", err)
	}
	if month.UnmeteredAttempts < 1 {
		t.Errorf("month reports %d unpriced attempts, want at least 1", month.UnmeteredAttempts)
	}
}

// TestTheCrossingTranslationHaltsTheMonth asserts the cap end of the
// budget: the translation that takes the month over is still recorded - the
// money was spent before this module got a say - and the month is latched
// so the caller stops before the next provider call.
func TestTheCrossingTranslationHaltsTheMonth(t *testing.T) {
	t.Parallel()
	tx := ledgerTx(t)
	ctx := context.Background()

	ledger := translation.NewLedger(tx)
	before, err := ledger.RecordUnbilledSpend(ctx, translation.Spend{})
	if err != nil {
		t.Fatalf("reading the month under lock: %v", err)
	}
	// The cap is set relative to whatever the month already holds, so the
	// test asserts a crossing rather than an accident of history.
	writer := translation.NewWriter(tx, translation.Caps{
		PerArticleMicroUSD: 20_000,
		MonthlyMicroUSD:    before + 30_000,
	})

	under, err := writer.Record(ctx, translation.Record{
		SourceItemID: seedItem(t, tx),
		TargetLocale: "de",
		Result:       affordableResult(20_000, 0),
	})
	if err != nil {
		t.Fatalf("first translation: %v", err)
	}
	if under.Halted() {
		t.Fatalf("the month halted at %d micro-USD, below its cap of %d", under.MonthToDateMicroUSD, before+30_000)
	}

	crossing, err := writer.Record(ctx, translation.Record{
		SourceItemID: seedItem(t, tx),
		TargetLocale: "de",
		Result:       affordableResult(20_000, 0),
	})
	if err != nil {
		t.Fatalf("the crossing translation must still be recorded: %v", err)
	}
	if crossing.TranslationID == uuid.Nil {
		t.Error("the crossing translation was not written: the money was spent before the cap could refuse it")
	}
	if !crossing.Halted() {
		t.Fatalf("the month did not halt at %d micro-USD against a cap of %d", crossing.MonthToDateMicroUSD, before+30_000)
	}

	// The halt is announced once, by the database, off the latching update.
	var events int
	if err := tx.QueryRow(ctx,
		`select count(*) from domain_event
		  where type = 'pipeline.halted'
		    and (payload->>'month')::date = date_trunc('month', now() at time zone 'utc')::date
		    and (payload->>'halted_at')::timestamptz = $1`, crossing.HaltedAt).Scan(&events); err != nil {
		t.Fatalf("querying pipeline.halted events: %v", err)
	}
	if events != 1 {
		t.Fatalf("pipeline.halted events for this halt = %d, want exactly 1", events)
	}

	// A later translation in a halted month is still recorded - refusing
	// it would destroy the record without saving a cent - and the month
	// keeps the time it originally halted at.
	after, err := writer.Record(ctx, translation.Record{
		SourceItemID: seedItem(t, tx),
		TargetLocale: "de",
		Result:       affordableResult(1_000, 0),
	})
	if err != nil {
		t.Fatalf("translation after the halt: %v", err)
	}
	if !after.HaltedAt.Equal(crossing.HaltedAt) {
		t.Errorf("halt time = %v, want the original %v: the latch is closed", after.HaltedAt, crossing.HaltedAt)
	}
	if err := tx.QueryRow(ctx,
		`select count(*) from domain_event
		  where type = 'pipeline.halted'
		    and (payload->>'month')::date = date_trunc('month', now() at time zone 'utc')::date`).Scan(&events); err != nil {
		t.Fatalf("querying pipeline.halted events: %v", err)
	}
	if events != 1 {
		t.Fatalf("pipeline.halted events this month = %d, want exactly 1", events)
	}
}

// TestAnUnrecordableTranslationStillCosts asserts the rule holds for every
// refusal, not just the ones this module has a name for: whatever the
// database declines to store - here an unknown locale - the call that
// produced it was billed, and the ledger says so.
func TestAnUnrecordableTranslationStillCosts(t *testing.T) {
	t.Parallel()
	tx := ledgerTx(t)
	ctx := context.Background()
	itemID := seedItem(t, tx)

	ledger := translation.NewLedger(tx)
	before, err := ledger.RecordUnbilledSpend(ctx, translation.Spend{})
	if err != nil {
		t.Fatalf("reading the month under lock: %v", err)
	}
	writer := translation.NewWriter(tx, translation.Caps{PerArticleMicroUSD: 20_000, MonthlyMicroUSD: before + 10_000_000})

	out, err := writer.Record(ctx, translation.Record{
		SourceItemID: itemID,
		TargetLocale: "zz", // not in the language reference table
		Result:       affordableResult(1_900, 0),
	})
	if err == nil {
		t.Fatal("Record() accepted a translation into an unknown locale")
	}
	if errors.Is(err, translation.ErrAlreadyTranslated) {
		t.Errorf("Record() classified an unknown locale as a duplicate: %v", err)
	}
	if out.TranslationID != uuid.Nil {
		t.Errorf("Record() reported translation %s for a row the database refused", out.TranslationID)
	}
	if got := out.MonthToDateMicroUSD - before; got != 1_900 {
		t.Errorf("month moved by %d micro-USD, want 1900: the provider billed for the call the database would not store", got)
	}
}

// TestRecordRefusesAnUnconfiguredBudget guards the wiring mistake: an unset
// budget must not read as "everything is over the ceiling and the month is
// over its cap".
func TestRecordRefusesAnUnconfiguredBudget(t *testing.T) {
	t.Parallel()
	tx := ledgerTx(t)
	writer := translation.NewWriter(tx, translation.Caps{})

	_, err := writer.Record(context.Background(), translation.Record{
		SourceItemID: seedItem(t, tx),
		TargetLocale: "de",
		Result:       affordableResult(1_500, 0),
	})
	if !errors.Is(err, translation.ErrCapsNotConfigured) {
		t.Fatalf("Record() error = %v, want ErrCapsNotConfigured", err)
	}
}
