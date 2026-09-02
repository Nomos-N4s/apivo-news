// Wiring the earnings lifecycle: the scheduled job that turns what the
// sweeps stored into what a member is owed (#435, US7).
//
// A file of its own for the reason catalogue.go is one: it is the piece of
// the composition root that decides whether the crediting job exists, and
// how the HOLD_* configuration reaches the rules that run inside it (T118).
// The job is built beside the routes rather than beside the scheduler
// because it must post in THE SAME LEDGER the wallet reads and the
// withdrawal path debits - under the in-process driver a second ledger
// would be a second set of balances, credited by the job and invisible to
// the member.

package main

import (
	"context"
	"log/slog"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
	"github.com/Nomos-N4s/apivo-news/internal/platform/scheduler"
)

// newEarningsLifecycle assembles the crediting job, or reports nil and the
// environment key it still lacks.
//
// The one key it can lack is the house account earnings are paid out of.
// Production cannot start without it (config refuses), and the environments
// that can are the no-Docker loop and CI - there the job is not scheduled
// and the caller says so at ERROR, the stance the catalogue import takes on
// a missing brand: serve everything else, and never be quiet about what is
// not running, because a report nothing credits looks exactly like a report
// nothing has reported yet.
func newEarningsLifecycle(log *slog.Logger, cfg config.Config, db earnings.Beginner, ledger wallet.Ledger) (*earnings.Lifecycle, []string, error) {
	if cfg.Cashback.HouseAccounts.NetworkReceivable == "" {
		return nil, []string{"HOUSE_ACCOUNT_NETWORK_RECEIVABLE"}, nil
	}
	job, err := earnings.NewLifecycle(log, db, ledger, cfg.Cashback.HouseAccounts.NetworkReceivable,
		holdRules(cfg.Cashback.HoldRules))
	if err != nil {
		return nil, nil, err
	}
	return job, nil, nil
}

// holdRules carries the HOLD_* configuration into the rules, field for
// field. Config has already refused a half-configured set; the rules check
// again at construction, which is cheap and means neither has to trust the
// other.
func holdRules(c config.HoldRulesConfig) earnings.HoldRules {
	return earnings.HoldRules{
		SharedContextAccounts: c.SharedContextAccounts,
		SharedContextWindow:   c.SharedContextWindow,
		NewAccountAge:         c.NewAccountAge,
		SaleCap:               c.SaleCap,
		MemberVelocity:        c.MemberVelocity,
		MemberVelocityWindow:  c.MemberVelocityWindow,
	}
}

// registerLifecycle puts the crediting job on the scheduler and answers how
// many jobs that added, for the capacity check - extracted for the reason
// registerSettlement is. A nil job is not an error: it means the
// authenticated surface was not built, cashback is off, or the house account
// is unnamed, and the caller has already said which.
func registerLifecycle(ctx context.Context, log *slog.Logger, jobs *scheduler.Scheduler, job *earnings.Lifecycle) (int, error) {
	if job == nil {
		return 0, nil
	}
	if err := job.Register(jobs); err != nil {
		return 0, err
	}
	rules := job.Rules().Active()
	log.InfoContext(ctx, "earnings lifecycle registered",
		"job", earnings.LifecycleJobName, "interval", earnings.LifecycleInterval,
		"hold_rules", rules)
	if len(rules) == 0 {
		// Not an error: alpha may run without rules. But a deployment that
		// meant to have them and set none would credit every self-dealing
		// pattern straight to pending, so it is said where it will be read.
		log.WarnContext(ctx, "no HOLD_* rule is configured, so every credit opens pending and nothing is held for review")
	}
	return 1, nil
}
