package wallet

// The continuous half of the C-1 mitigation. The double-entry invariant is
// the one guarantee that lives outside our own schema (ADR-0002, the named
// Principle VIII exception), and the constitution's price for that is a
// check that is "a real SQL query over real rows", failing loudly and run
// continuously - not once in CI. Migration 0016 provides the query:
// cashback.ledger_zero_sum, one row per ledger currency with its net in
// minor units, resolving for itself which schema the ledger lives in
// (0020). This file is the running of it.
//
// Four outcomes, four behaviours, and the distinctions carry the whole
// point:
//
//   - Every currency nets to zero: one debug line, saying how many
//     currencies it vouches for. SC-003 holding is the steady state, not
//     an event.
//   - The view had no rows at all: nothing was summed, and the run says
//     exactly that in a different sentence, because "the ledger is clean"
//     and "there was no ledger here to sum" must never be the same line.
//     The vacuous pass is still a pass - a database with no co-located
//     ledger has no postings to disagree about - and it stays honest only
//     because 0016/0020 RAISE whenever a ledger that should be visible is
//     not.
//   - A currency does not net to zero: money was created or destroyed
//     inside the ledger, which is an incident, not a metric (SC-003). The
//     run says so at ERROR with the per-currency deltas and returns
//     [ErrOutOfBalance] - and says so AGAIN on every following run for as
//     long as the imbalance stands, because an incident logged once and
//     scrolled away is an incident forgotten. It never stops the process:
//     a wrong ledger is precisely when the check must keep watching.
//   - The query itself fails: the check could not see the postings it
//     exists to sum, and this run reports that as a failure of the CHECK -
//     never as "no imbalance", which is the silent pass the whole
//     invariant exists to prevent.
//
// Where the check looks is not left to luck. 0020 resolves the ledger
// schema to `blnk` unless someone names another - right for a co-located
// Blnk, and right for a database holding no ledger at all - but the
// Postgres exit route keeps its ledger in this database's own `ledger`
// schema (0022), which the default never resolves. A check left on the
// default would sum nothing over the one ledger it is guaranteed to share
// a database with, and pass vacuously while that ledger drifts. So the
// composition root hands the check the schema its LEDGER_DRIVER choice
// implies, and each run pins 0020's setting to it, transaction-locally,
// for exactly as long as the run lasts.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Nomos-N4s/apivo-news/internal/platform/scheduler"
)

const (
	// ZeroSumJobName identifies the check in the scheduler: in its log
	// lines, and as the name of its fleet-wide lock. It must stay stable
	// across releases - two instances exclude each other only while they
	// agree on it.
	ZeroSumJobName = "ledger-zero-sum"
	// ZeroSumInterval is how long the ledger may go unexamined, and it is
	// a constant on purpose, not a knob. The run is one aggregate read
	// over the ledger's balance rows, cheap enough that nothing is bought
	// by spacing it out - and a cadence an environment variable could set
	// is a cadence an environment variable could set to never, which for
	// the one continuous verification of C-1 (Principle VIII, SC-003
	// "verified continuously") would be an off switch for a constitutional
	// invariant dressed up as tuning. A minute keeps an imbalance
	// something the process watches for rather than something it
	// discovers.
	ZeroSumInterval = time.Minute
	// zeroSumTimeout bounds one run. The check is a single aggregate read
	// over the ledger's balance rows, so a run that is still going after a
	// minute is not summing, it is wedged - and a wedged run must free its
	// lock for the next tick rather than silently stopping the check
	// fleet-wide.
	zeroSumTimeout = time.Minute
)

// ErrOutOfBalance reports a zero-sum run that found at least one currency
// whose ledger postings do not net to zero (C-1). It is distinct from
// [ErrUnbalanced], which refuses a single transfer before any I/O: this
// error means the refusals were not enough - money was created or
// destroyed inside the ledger anyway - and it is declared here so callers
// can tell "the ledger is wrong" from "the check could not look", which
// demand different responses. It is the check's error, not the Ledger
// port's: contracts/ports.md §1 fixes the port's error vocabulary, no port
// method returns this one, and it leaves the package only through
// [ZeroSumCheck.Run] - it lives beside the port because T046 keeps the
// watchdog next to the thing it watches, not because the port grew a case.
var ErrOutOfBalance = errors.New("wallet: the ledger does not net to zero per currency")

// zeroSumQuery is the C-1 read, character for character the query the
// platform drill proves can see a planted imbalance through
// (internal/platform/db/cashback_provenance_test.go, the readable-ledger
// drill). The two only vouch for each other while they run the same
// string. The `where net_minor <> 0` a reader might expect is applied in
// Go instead, deliberately: the row count is the whole difference between
// "N currencies verified clean" and "nothing here was summed", and a WHERE
// clause would throw that difference away before the run could report
// which of the two it is vouching for.
const zeroSumQuery = `select currency, net_minor from cashback.ledger_zero_sum`

// TxBeginner is the narrow slice of database access the check needs:
// opening the short read-only transaction one run lives in. A transaction
// rather than a bare query, because the schema pin must hold for exactly
// one run - set_config's transaction-local form is the only way to move
// 0020's resolution without the new value escaping through a pooled
// connection into every other query in the process. *pgxpool.Pool
// satisfies it, and so does pgx.Tx - a nested Begin is a savepoint - which
// is how the tests point the check at a stand-in ledger that lives and
// dies inside a single rolled-back transaction, exactly the way the
// platform drills build theirs.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// ZeroSumCheck is the continuous C-1 check. Construct it with
// NewZeroSumCheck, hand it to the process's scheduler with Register, and
// the scheduler drives Run on the package's own cadence under a
// fleet-wide lock. Run is exported so tests - and an operator wanting one
// answer now - can drive a single pass without a scheduler around it.
type ZeroSumCheck struct {
	log          *slog.Logger
	db           TxBeginner
	ledgerSchema string
}

// NewZeroSumCheck builds the check on the process's connection pool - the
// same pool the HTTP server uses, per the composition root - or on any
// narrower handle satisfying TxBeginner. ledgerSchema is where the
// composition root's ledger driver says a co-located ledger lives: the
// exit route's `ledger` (0022) under LEDGER_DRIVER=postgres, and ""
// otherwise, which leaves each run on 0020's own resolution - Blnk's
// `blnk` when it shares the database, and nothing at all when no ledger is
// co-located, where vacuously clean is the honest answer. A named schema
// is pinned for each run, and 0020 then RAISES if it is absent: a check
// that was told where the ledger is must fail when it is not there, never
// report that nothing summed to nothing.
func NewZeroSumCheck(log *slog.Logger, db TxBeginner, ledgerSchema string) *ZeroSumCheck {
	return &ZeroSumCheck{log: log, db: db, ledgerSchema: ledgerSchema}
}

// Register puts the check on s as the ledger-zero-sum job. The
// registration lives here rather than in the composition root so that the
// job's identity - its name, its cadence, its timeout - has one owner, and
// a second wiring site could not register the same check under a name that
// no longer excludes the first, or on a cadence that quietly stretches how
// long the ledger goes unexamined.
func (c *ZeroSumCheck) Register(s *scheduler.Scheduler) error {
	return s.Register(scheduler.Job{
		Name:     ZeroSumJobName,
		Interval: ZeroSumInterval,
		Timeout:  zeroSumTimeout,
		Run:      c.Run,
	})
}

// Run is one pass of the check: execute the C-1 read, report every
// currency that does not net to zero, and come back clean only when it
// found none - saying, even then, whether that verdict covers real
// currencies or nothing at all.
//
// A violation is returned as an error wrapping [ErrOutOfBalance] on top of
// the per-currency ERROR lines, deliberately: to the scheduler a run that
// saw the invariant broken is a failed run, so the imbalance is loud on
// both channels and stays loud every tick until the incident is resolved.
// A query failure is returned as an error that is NOT ErrOutOfBalance,
// equally deliberately: the view raises precisely when the check cannot
// see the postings it exists to sum, and a raise reported as "no
// imbalance" would be the check blinding itself - the failure 0016 and
// 0020 exist to prevent.
func (c *ZeroSumCheck) Run(ctx context.Context) error {
	// One transaction per run, always rolled back: the run only reads, and
	// the rollback is what confines the schema pin below to this run.
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("wallet: the C-1 zero-sum check could not open its transaction, which is a failure of the check and never a clean ledger: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if c.ledgerSchema != "" {
		if _, err := tx.Exec(ctx,
			`select set_config('cashback.ledger_schema', $1, true)`, c.ledgerSchema); err != nil {
			return fmt.Errorf("wallet: the C-1 zero-sum check could not point itself at the %q schema, which is a failure of the check and never a clean ledger: %w", c.ledgerSchema, err)
		}
	}

	rows, err := tx.Query(ctx, zeroSumQuery)
	if err != nil {
		return fmt.Errorf("wallet: the C-1 zero-sum check could not run, which is a failure of the check and never a clean ledger: %w", err)
	}
	defer rows.Close()

	// int64 minor units and the ledger's own currency code, exactly as the
	// view reports them (C-6). The currency stays a string rather than
	// being parsed into money.Currency: if the ledger ever held a balance
	// under a code our validation would refuse, that row is the most
	// important one to report, not one to choke on.
	type delta struct {
		currency string
		netMinor int64
	}
	var currencies int
	var broken []delta
	for rows.Next() {
		var d delta
		if err := rows.Scan(&d.currency, &d.netMinor); err != nil {
			return fmt.Errorf("wallet: the C-1 zero-sum check could not read its own result: %w", err)
		}
		currencies++
		if d.netMinor != 0 {
			broken = append(broken, d)
		}
	}
	// The view computes through set-returning functions, so the RAISE a
	// blinded check produces can surface here, mid-iteration, rather than
	// from Query - it is the same failure of the check either way.
	if err := rows.Err(); err != nil {
		return fmt.Errorf("wallet: the C-1 zero-sum check could not run, which is a failure of the check and never a clean ledger: %w", err)
	}

	if len(broken) == 0 {
		// Debug, not Info, in both clean shapes: with the check on a
		// minute cadence these lines are the steady state, and at Info
		// they would bury the one run that ever says anything else. But
		// never the SAME line: a run that verified currencies and a run
		// that summed nothing are different facts, and a reader of either
		// must be able to tell which one the check is vouching for.
		if currencies == 0 {
			c.log.DebugContext(ctx, "the C-1 check summed no currencies: no co-located ledger is visible, or it holds no balances yet - vacuously clean, kept honest by 0016/0020 raising whenever a ledger that should be visible is not",
				"job", ZeroSumJobName)
			return nil
		}
		c.log.DebugContext(ctx, "the ledger nets to zero in every currency (C-1)",
			"job", ZeroSumJobName, "currencies", currencies)
		return nil
	}

	deltas := make([]string, len(broken))
	for i, d := range broken {
		// One record per currency, with the delta in structured
		// attributes: an alert can match on the message and read the
		// numbers without parsing prose.
		c.log.ErrorContext(ctx, "C-1 violated: money was created or destroyed inside the ledger; this is an incident, not a metric",
			"job", ZeroSumJobName, "currency", d.currency, "net_minor", d.netMinor)
		deltas[i] = fmt.Sprintf("%s %+d", d.currency, d.netMinor)
	}
	return fmt.Errorf("%w: %s", ErrOutOfBalance, strings.Join(deltas, ", "))
}
