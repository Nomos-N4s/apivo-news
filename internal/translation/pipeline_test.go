package translation

// Unit tests for the pipeline's construction-time validation, its failure
// classification, and - on a scripted in-memory database - the
// money-handling paths of the cycle itself: a ledger that refuses the
// booking, a provider-wide failure, a cap crossed mid-cycle. The real SQL
// runs against the real schema in pipeline_integration_test.go; these
// tests pin the ORDER of operations and what each failure means, and they
// count locally, where no database runs.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// validPipelineConfig is a configuration NewPipeline has no objection to.
func validPipelineConfig() PipelineConfig {
	return PipelineConfig{
		Interval:      time.Minute,
		ReaderLocales: []string{"el", "de"},
		Caps:          Caps{PerArticleMicroUSD: 20_000, MonthlyMicroUSD: 25_000_000},
	}
}

func TestNewPipelineRefusesAConfigurationItCannotRun(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)

	tests := []struct {
		name   string
		mutate func(*PipelineConfig)
	}{
		// A missing budget is not an unlimited one: a pipeline without
		// caps must never reach a provider.
		{"no caps", func(c *PipelineConfig) { c.Caps = Caps{} }},
		// An unregistered prompt version would be recorded as the lineage
		// of every row written with it - and would be a lie.
		{"unknown prompt version", func(c *PipelineConfig) { c.PromptVersion = "v0-never-released" }},
		// No reader locales means no work could ever be eligible; running
		// anyway would poll the database every interval for nothing.
		{"no reader locales", func(c *PipelineConfig) { c.ReaderLocales = nil }},
		// Interval zero means "disabled", and a disabled pipeline is one
		// the composition root never constructs; accepting it here would
		// produce a loop that spins hot.
		{"zero interval", func(c *PipelineConfig) { c.Interval = 0 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := validPipelineConfig()
			tt.mutate(&cfg)
			if _, err := NewPipeline(log, nil, nil, cfg); err == nil {
				t.Fatalf("NewPipeline() accepted a configuration it cannot run: %+v", cfg)
			}
		})
	}

	t.Run("valid configuration defaults the rest", func(t *testing.T) {
		t.Parallel()
		p, err := NewPipeline(log, nil, nil, validPipelineConfig())
		if err != nil {
			t.Fatalf("NewPipeline() = %v, want nil", err)
		}
		if p.cfg.Limit != DefaultCycleLimit {
			t.Errorf("Limit defaulted to %d, want %d", p.cfg.Limit, DefaultCycleLimit)
		}
		if p.cfg.PromptVersion != CurrentPromptVersion {
			t.Errorf("PromptVersion defaulted to %q, want %q", p.cfg.PromptVersion, CurrentPromptVersion)
		}
	})
}

// TestFailureClassification pins which failures step to the next item and
// which end the cycle: misreading an item failure as provider-wide stops
// every translation over one bad feed entry, while the reverse pays the
// adapter's whole retry budget once per item against a host that is down.
func TestFailureClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		err          error
		wantItem     bool
		wantProvider bool
	}{
		{"invalid request", ErrInvalidRequest, true, false},
		{"invalid response", ErrInvalidResponse, true, false},
		{"timeout", ErrTimeout, true, false},
		{"auth", ErrAuth, false, true},
		{"rate limited", ErrRateLimited, false, true},
		{"unavailable", ErrUnavailable, false, true},
		// A SpendError classifies by what it wraps: the money is booked
		// separately, the verdict comes from the failure itself.
		{"spend error wrapping a response failure", &SpendError{Spend: Spend{CostMicroUSD: 5}, Err: ErrInvalidResponse}, true, false},
		{"spend error wrapping a rate limit", &SpendError{Spend: Spend{CostMicroUSD: 5}, Err: fmt.Errorf("wrapped: %w", ErrRateLimited)}, false, true},
		// Anything unclassified is infrastructure and fails the cycle.
		{"plain database error", errors.New("connection refused"), false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := itemFailure(tt.err); got != tt.wantItem {
				t.Errorf("itemFailure(%v) = %t, want %t", tt.err, got, tt.wantItem)
			}
			if got := providerFailure(tt.err); got != tt.wantProvider {
				t.Errorf("providerFailure(%v) = %t, want %t", tt.err, got, tt.wantProvider)
			}
		})
	}
}

// ---- a scripted in-memory database ----
//
// The cycle's database work is QueryRow-shaped everywhere that matters,
// and pgx.Row is a one-method interface, so a scripted fake can stage the
// exact states the money-handling paths need without a running Postgres.
// Statements are recognised by substring: these tests pin the order of
// operations and the meaning of each outcome, never the SQL text - the
// real statements run against the real schema in the integration tests.

// fakeDB is an in-memory PipelineDB: a work list, a month ledger the
// scripted statements move exactly as the schema's trigger would, and one
// switch that makes the ledger refuse to book.
type fakeDB struct {
	items []workItem

	monthSpent    int64
	monthAttempts int
	halted        bool
	haltedAt      time.Time

	// refuseSpendInsert, when set, fails every translation_spend upsert
	// with this error: the ledger that cannot book.
	refuseSpendInsert error
}

func (f *fakeDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	return f.answer(sql, args)
}

func (f *fakeDB) Begin(context.Context) (pgx.Tx, error) { return &fakeTx{db: f}, nil }

func (f *fakeDB) Query(_ context.Context, _ string, args ...any) (pgx.Rows, error) {
	// eligibleItems. The first page is the whole scripted work list; any
	// keyset continuation (a non-nil cursor, args[3]) is past its end.
	if args[3] != nil {
		return &fakeRows{}, nil
	}
	rows := &fakeRows{}
	for _, item := range f.items {
		rows.rows = append(rows.rows, []any{item.ID.String(), item.Title, item.RawBody, item.SourceLanguage, item.RetrievedAt})
	}
	return rows, nil
}

func (f *fakeDB) answer(sql string, args []any) pgx.Row {
	switch {
	case strings.Contains(sql, "pg_try_advisory_xact_lock"):
		return fakeRow{vals: []any{true}}
	case strings.Contains(sql, "select exists (select 1 from translation"):
		return fakeRow{vals: []any{false}}
	case strings.Contains(sql, "left join translation_spend"):
		// Ledger.ThisMonth.
		return fakeRow{vals: []any{monthStart(), f.monthSpent, f.monthAttempts, f.haltedAt}}
	case strings.Contains(sql, "insert into translation_spend"):
		// Ledger.RecordUnbilledSpend - the booking.
		if f.refuseSpendInsert != nil {
			return fakeRow{err: f.refuseSpendInsert}
		}
		f.monthSpent += args[0].(int64)
		f.monthAttempts += args[1].(int)
		return fakeRow{vals: []any{f.monthSpent}}
	case strings.Contains(sql, "insert into translation ("):
		// The writer's insert; moving the ledger with it is what the 0005
		// trigger does on the real schema.
		f.monthSpent += args[6].(int64)
		f.monthAttempts += args[7].(int)
		return fakeRow{vals: []any{uuid.NewString()}}
	case strings.Contains(sql, "update translation_spend"):
		// Ledger.Halt: latch once, at or over the cap, never twice.
		if f.halted || f.monthSpent < args[1].(int64) {
			return fakeRow{err: pgx.ErrNoRows}
		}
		f.halted = true
		f.haltedAt = time.Now()
		return fakeRow{vals: []any{f.haltedAt}}
	}
	return fakeRow{err: fmt.Errorf("fake database: unscripted statement: %s", sql)}
}

func monthStart() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// fakeTx is the fake's transaction: scoping is not what these tests pin
// (the rollback-discards-everything guarantees are integration territory),
// so it answers from the shared state and commits are no-ops. The embedded
// interface panics on anything the pipeline is not expected to call.
type fakeTx struct {
	pgx.Tx
	db *fakeDB
}

func (t *fakeTx) Begin(context.Context) (pgx.Tx, error) { return &fakeTx{db: t.db}, nil }
func (t *fakeTx) Commit(context.Context) error          { return nil }
func (t *fakeTx) Rollback(context.Context) error        { return nil }
func (t *fakeTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	return t.db.answer(sql, args)
}

// fakeRow scripts one pgx.Row: an error, or values assigned by type.
type fakeRow struct {
	vals []any
	err  error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return assignAll(dest, r.vals)
}

// fakeRows scripts pgx.Rows for the work-list query. Only the methods the
// pipeline's scan loop uses do anything.
type fakeRows struct {
	rows [][]any
	i    int
}

func (r *fakeRows) Next() bool { r.i++; return r.i <= len(r.rows) }
func (r *fakeRows) Scan(dest ...any) error {
	return assignAll(dest, r.rows[r.i-1])
}
func (r *fakeRows) Err() error                    { return nil }
func (r *fakeRows) Close()                        {}
func (r *fakeRows) CommandTag() pgconn.CommandTag { return pgconn.CommandTag{} }
func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription {
	return nil
}
func (r *fakeRows) Values() ([]any, error) {
	return nil, errors.New("fake rows: Values is not scripted")
}
func (r *fakeRows) RawValues() [][]byte { return nil }
func (r *fakeRows) Conn() *pgx.Conn     { return nil }

// assignAll writes scripted values into scan destinations, converting to
// the pgtype wrappers the production code scans into.
func assignAll(dest, vals []any) error {
	if len(dest) != len(vals) {
		return fmt.Errorf("fake row: %d destinations for %d values", len(dest), len(vals))
	}
	for i, d := range dest {
		switch d := d.(type) {
		case *bool:
			*d = vals[i].(bool)
		case *string:
			*d = vals[i].(string)
		case *int64:
			*d = vals[i].(int64)
		case *int:
			*d = vals[i].(int)
		case *time.Time:
			*d = vals[i].(time.Time)
		case *pgtype.Date:
			*d = pgtype.Date{Time: vals[i].(time.Time), Valid: true}
		case *pgtype.Timestamptz:
			at := vals[i].(time.Time)
			*d = pgtype.Timestamptz{Time: at, Valid: !at.IsZero()}
		default:
			return fmt.Errorf("fake row: no assignment for destination %T", d)
		}
	}
	return nil
}

// unitTranslator answers by source title, like the integration tests'
// scripted translator: a Result, an error, or an unscripted plain
// invalid-response failure that costs nothing.
type unitTranslator struct {
	calls   []Request
	results map[string]Result
	errs    map[string]error
}

func (u *unitTranslator) Translate(_ context.Context, req Request) (Result, error) {
	u.calls = append(u.calls, req)
	if err, ok := u.errs[req.SourceTitle]; ok {
		return Result{}, err
	}
	if res, ok := u.results[req.SourceTitle]; ok {
		return res, nil
	}
	return Result{}, fmt.Errorf("unit translator: unscripted item %q: %w", req.SourceTitle, ErrInvalidResponse)
}

func (u *unitTranslator) callsFor(title string) int {
	n := 0
	for _, req := range u.calls {
		if req.SourceTitle == title {
			n++
		}
	}
	return n
}

// unitItem is one scripted work item with a body that derives an extract.
func unitItem(title string, retrievedAt time.Time) workItem {
	return workItem{
		ID:             uuid.New(),
		Title:          title,
		RawBody:        "Σώμα με αρκετό κείμενο για ένα απόσπασμα.",
		SourceLanguage: "el",
		RetrievedAt:    retrievedAt,
	}
}

// unitResult is a provider answer the budget has no objection to.
func unitResult(costMicroUSD int64) Result {
	return Result{
		Headline:      "Übersetzte Überschrift",
		Extract:       "Übersetzter Auszug mit genug Text für einen Anriss.",
		Model:         "fake-model-1",
		PromptVersion: CurrentPromptVersion,
		Spend:         Spend{CostMicroUSD: costMicroUSD},
	}
}

func newUnitPipeline(t *testing.T, db PipelineDB, translator Translator, caps Caps) *Pipeline {
	t.Helper()
	p, err := NewPipeline(slog.New(slog.DiscardHandler), db, translator, PipelineConfig{
		Interval:      time.Minute,
		ReaderLocales: []string{"de"},
		Caps:          caps,
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

// TestABookingFailureDominatesAndEndsTheCycle pins the shape of the worst
// failure the cycle can meet: the provider billed for a failed call and
// the database then refused to book that spend. The error the cycle
// surfaces must be the BOOKING failure - the provider's sentinel quoted
// as text, never wrapped - because classifying it by the provider error
// would log a warning and walk on with a billed call recorded nowhere.
// And it must end the cycle: a database that cannot book this call's
// spend cannot book the next item's either.
func TestABookingFailureDominatesAndEndsTheCycle(t *testing.T) {
	t.Parallel()

	refusal := errors.New("translation_spend refuses every insert today")
	db := &fakeDB{refuseSpendInsert: refusal}
	now := time.Now()
	billed := unitItem("Billed but unbookable", now)
	unreached := unitItem("Never reached", now.Add(-time.Minute))
	db.items = []workItem{billed, unreached}

	translator := &unitTranslator{errs: map[string]error{
		billed.Title: &SpendError{Spend: Spend{CostMicroUSD: 700}, Err: ErrInvalidResponse},
	}}
	pipeline := newUnitPipeline(t, db, translator, Caps{PerArticleMicroUSD: 20_000, MonthlyMicroUSD: 25_000_000})

	err := pipeline.RunOnce(context.Background())
	if err == nil {
		t.Fatal("RunOnce() = nil: a billed call the ledger refused to book must fail the cycle")
	}
	if !errors.Is(err, refusal) {
		t.Errorf("RunOnce() = %v, want the booking failure in the chain", err)
	}
	if errors.Is(err, ErrInvalidResponse) {
		t.Errorf("RunOnce() = %v: the provider's sentinel must be quoted text, not wrapped - wrapping is what let the cycle misread a storage failure as one item's problem", err)
	}
	if itemFailure(err) || providerFailure(err) {
		t.Errorf("RunOnce() error classifies as item or provider failure; a booking failure must stay unclassified so the cycle ends: %v", err)
	}
	if !strings.Contains(err.Error(), "NOT on the ledger") {
		t.Errorf("RunOnce() = %v, want the unbooked spend named for the operator", err)
	}
	if got := translator.callsFor(unreached.Title); got != 0 {
		t.Errorf("the item after the booking failure reached the provider %d time(s), want none: the cycle must end", got)
	}
}

// TestAProviderWideFailureEndsTheCycleAfterOneCall pins RunOnce's
// provider-failure arm: the first pair meets a provider-wide failure, the
// cycle ends without error - the next interval retries - and no later
// item pays the adapter's retry budget against the same broken host.
func TestAProviderWideFailureEndsTheCycleAfterOneCall(t *testing.T) {
	t.Parallel()

	db := &fakeDB{}
	now := time.Now()
	down := unitItem("Provider is down for this one", now)
	unreached := unitItem("Never reached behind the outage", now.Add(-time.Minute))
	db.items = []workItem{down, unreached}

	translator := &unitTranslator{errs: map[string]error{down.Title: ErrUnavailable}}
	pipeline := newUnitPipeline(t, db, translator, Caps{PerArticleMicroUSD: 20_000, MonthlyMicroUSD: 25_000_000})

	if err := pipeline.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() = %v, want nil: a provider-wide failure ends the cycle quietly for the next interval", err)
	}
	if got := translator.callsFor(down.Title); got != 1 {
		t.Errorf("the failing pair was called %d time(s), want exactly once", got)
	}
	if got := translator.callsFor(unreached.Title); got != 0 {
		t.Errorf("the item behind the outage reached the provider %d time(s), want none", got)
	}
}

// TestACapCrossedMidCycleStopsBeforeTheNextCall pins the halt-mid-cycle
// stop: the call that crosses the monthly cap latches the halt, and the
// cycle makes NO further provider call - the very next pair is already
// too late to pay for.
func TestACapCrossedMidCycleStopsBeforeTheNextCall(t *testing.T) {
	t.Parallel()

	db := &fakeDB{}
	now := time.Now()
	crossing := unitItem("The call that crosses the cap", now)
	unreached := unitItem("One call too many", now.Add(-time.Minute))
	db.items = []workItem{crossing, unreached}

	translator := &unitTranslator{results: map[string]Result{
		crossing.Title:  unitResult(1_000),
		unreached.Title: unitResult(1_000),
	}}
	pipeline := newUnitPipeline(t, db, translator, Caps{PerArticleMicroUSD: 1_000, MonthlyMicroUSD: 1_000})

	if err := pipeline.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() = %v, want nil", err)
	}
	if got := translator.callsFor(crossing.Title); got != 1 {
		t.Errorf("the crossing pair was called %d time(s), want exactly once", got)
	}
	if got := translator.callsFor(unreached.Title); got != 0 {
		t.Errorf("a pair after the cap was crossed reached the provider %d time(s), want none: the halt must stop the cycle", got)
	}
	if !db.halted {
		t.Error("the month was never latched halted: the crossing call must latch it")
	}
}
