# Architecture Decision Records

The constitution states that architecture constraints are *"decided through
an ADR process; implement, do not re-litigate"*. This directory is that
process made explicit.

## Format

Each record is `NNNN-kebab-title.md` and carries: Status, Context, Decision,
Consequences, Alternatives considered (with the reason each was rejected),
and Revisit triggers. A record is never edited once **Accepted** — it is
superseded by a new record that names it.

## Status values

| Status | Meaning |
|---|---|
| Proposed | Written, awaiting founder acceptance. Not implementable. |
| Accepted | Founder-ratified. Implement; do not re-litigate. |
| Superseded by NNNN | Replaced. Kept for the historical record. |
| Rejected | Considered and declined. Kept so it is not re-proposed. |

## Index

| ADR | Title | Status |
|---|---|---|
| [0001](0001-super-app-architecture.md) | Super-app architecture: monorepo, product-scoped modular monoliths, event integration | Accepted |
| [0002](0002-cashback-money-substrate.md) | Cashback money substrate: adopt the Blnk open-source ledger | Accepted |
| [0003](0003-affiliate-network-integration.md) | Affiliate network integration: consumer-defined adapters, fixtures first | Accepted |
| [0004](0004-white-label-rebranding.md) | White-label architecture: one brand configuration, zero brand literals | Accepted |
| [0005](0005-cashback-stack.md) | Cashback stack: no second stack — Go and Astro, as today | Accepted |

## Relationship to Spec Kit

ADRs hold decisions that outlive a single feature. Spec Kit's
`specs/<feature>/research.md` holds the feature-local decisions and cites
the ADR whenever a decision is repository-wide. When the two disagree, the
ADR wins.
