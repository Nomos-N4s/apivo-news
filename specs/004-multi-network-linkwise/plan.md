# Implementation Plan: Many Affiliate Networks — Linkwise Beside Awin

**Branch**: `xcoder/003-multi-network` | **Date**: 2026-09-03 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `/specs/004-multi-network-linkwise/spec.md`

> **PARTIAL GATE.** Founder question **Q1** is amended by the request
> itself — the networks are Awin *and* Linkwise — and that amendment is
> recorded in [spec.md](spec.md#q1-is-amended-by-this-feature). Four
> questions are **open**: Q10 (route arbitration), Q11 (Linkwise account and
> API access), Q12 (does Awin stay), Q13 (currency posture). Each has a safe
> default, and **none blocks the plan below except for the adapter phases**:
> Phase 3 cannot start for a network until that network has a recorded
> response (FR-105), which for Linkwise is Q11. Everything in Phases 0–2 is
> knowable today and is what makes the adapters cheap when the shapes
> arrive.

## Summary

The product is to work with **Linkwise as well as Awin**. Reading the tree
first changed what that means: **the product does not integrate Awin
either.** `*awin.Client` has no `FetchTransactions` and no `Limits()`, so it
does not satisfy `networks.Network`, and `cmd/apivo`'s adapter switch ships
only the fixture — `NETWORK_DRIVER=awin` fails at startup
([research.md](research.md#1-the-finding-that-reframes-the-feature) §1).

That gap is deliberate and recorded: `002/research.md` §9.2 refused to map a
money field from documentation Awin does not publish. Linkwise is in exactly
the same position — no public endpoint, no auth scheme, no field names
([research.md](research.md#5-linkwise) §5) — so the same refusal applies to
it, and the plan is built so that refusal costs nothing.

The work therefore splits three ways, and only the third is blocked:

1. **Make the seam real.** One registry of shipped drivers instead of two
   disagreeing switches; configuration that can hold N networks; a
   composition root that connects each one it is given; per-network job
   names so two networks do not fight over one lock.
2. **Make the seam safe.** Three findings misbehave *silently* rather than
   loudly the day a second network exists: one click can back two credits;
   the published route is chosen at first import and never revisited; the
   conformance suite did not assert the rule that says every yielded value
   validated. The third is already closed
   ([#497](https://github.com/Nomos-N4s/apivo-news/pull/497)); the first two
   are database work, per Principle VIII.
3. **Write the adapters**, each held to FR-105: no money field mapped from
   anything but a recorded, redacted response committed under that adapter's
   own `testdata/`.

Nothing here is a rebuild. The schema was designed multi-network from the
start — `cashback.network` is a table, `merchant_network` is one route per
network per retailer, and `merchant_network_one_preferred` is already a
partial unique index. The port is already consumer-defined and already keyed
by `PublisherAccount` rather than by network. What is single-network is
**the composition root and four files under it**, which is the correct place
for the assumption to have been, and the reason SC-008 is achievable at all.

## Technical Context

**Language/Version**: Go 1.26 — unchanged. No frontend work in this feature.

**Primary Dependencies**: unchanged. No new module. Both adapters speak HTTP
and JSON through the existing `networks` vocabulary.

**Storage**: Postgres, `cashback` schema. Migrations **0033** onward; `0032`
is the current head.

**Testing**: `go test` against real Postgres for every database-enforced
rule, asserting the **database** refuses the illegal state by SQLSTATE
(Principle VIII); the shared conformance suite run against **every** adapter
the binary ships, not only the fixture; the 90% coverage gate unchanged.

**Target Platform**: unchanged.

**Performance Goals**: unchanged. Two networks doubles a small number.
Connection budget is the one real arithmetic: the scheduler reserves 2
connections per registered job plus 2, so a second network's forward,
trailing and catalogue jobs move the floor — `pool_max_conns` is sized in
Phase 1 and asserted by the capacity check at startup (FR-095).

**Constraints**: C-1..C-7 unchanged and non-negotiable. Two acquire a second
meaning: **C-2** must additionally mean *the click's own network*, and
**C-6** keeps its one-currency-per-report rule, which is what makes Q13 a
migration rather than an adapter tweak.

**Scale/Scope**: alpha, unchanged. Two networks, one brand, one currency
assumed (see Q13 and [research.md](research.md) §5.4).

**Unknowns**: everything about the Linkwise wire format, exhaustively listed
in [research.md](research.md) §5.2. They are **not resolved here and must
not be guessed**. They are quarantined behind FR-105 so that they block one
phase and nothing else.

## Constitution Check

*Delta against the 002 check, which passed under v1.2.0. Only rows whose
evidence changes are listed; every other row is unchanged and still passes.*

| Principle | Status | Evidence in this plan |
|---|---|---|
| I. Sole authorship, signed commits | PASS | Unchanged. Branch is `xcoder/003-multi-network`; `make ref-lint` and `scripts/lint-commit-authors.sh` run before every push |
| III. I-2 provenance at retrieval | PASS (strengthened) | Every adapter records retrieval time, **its own** publisher account and the query window — with two adapters, the account is what tells two provenance rows apart |
| VI. I-5 traceability | PASS (strengthened) | The `cashback.provenance` view already joins through `merchant_network`, so it names the network per row without change |
| VIII. DB-enforced invariants | PASS (extended) | Two new database rules, each with a SQLSTATE-asserting test: **one click backs at most one credit** (0033), and **a published route must be usable** (0034). Both are today's application-level assumptions made structural |
| IX. Money is double entry, evidence-backed, exactly once | PASS (sharpened) | C-2 gains its missing half: a credit's click must belong to the **reporting** network. Enforced in the query *and* in the database, because a query is a habit and a constraint is a rule |
| Architecture: adapters swappable in < 5 engineer-days | **PASS — and finally proved** | The constitution requires every external dependency be proved by *a second working implementation in the repository*, and names affiliate network adapters. Today the second implementation is a fixture. This feature makes it a second **real** network, which is what the clause actually asks for |
| Architecture: module boundaries | PASS (guarded) | The registry that ends the two-switch disagreement lives in the **composition root**, not in the domain, because `internal/arch/network_isolation_test.go` rule A forbids any domain package naming an adapter. Putting it anywhere else fails that test, and correctly |
| Quality bar (90% Go, real Postgres, strict lint) | PASS | Unchanged. The conformance suite becomes a table over shipped adapters, so a new adapter that is not run by it is a compile-time omission rather than a silent one |

**Verdict**: **PASS**, with no new violation and one long-standing one
retired — the "second working implementation" clause for network adapters
stops being satisfied by a test double.

## Project Structure

### Documentation (this feature)

```text
specs/004-multi-network-linkwise/
├── spec.md              # feature specification (+ founder questions Q10–Q13)
├── plan.md              # this file
├── research.md          # current-state audit, money findings, Linkwise
├── data-model.md        # schema deltas 0033–0035 only
├── quickstart.md        # bringing a second network up locally
├── contracts/
│   ├── ports.md         # what the port contract gains, and rule 7's repair
│   ├── config.md        # the N-network configuration surface
│   └── http-api.md      # the operator endpoints this feature adds
└── tasks.md             # T200 onward
```

### Source code (delta only — everything unlisted is untouched)

```text
cmd/apivo/
├── networks.go              # the two switches collapse into one registry
├── connect_network.go       #   ...read by both, so seedable ⇒ servable
├── registry.go              # NEW: the one table of shipped drivers
└── cashback.go              # connects each configured network, not one

internal/platform/config/
└── cashback.go              # NetworkConfig becomes a list, keyed by driver

internal/cashback/
├── networks/
│   ├── conformance_test.go  # runs over every shipped adapter
│   ├── sweeps.go            # (already per-account — unchanged, verified)
│   ├── awin/                # gains FetchTransactions + Limits (gated)
│   └── linkwise/            # NEW package (gated on Q11)
├── catalogue/
│   └── schedule.go          # ImportJobName constant becomes per-account
└── clickout/
    └── queries/click.sql    # GetClickByRef gains a network predicate

internal/platform/db/migrations/
├── 0033_click_backs_one_credit.{up,down}.sql
├── 0034_preferred_route_must_be_publishable.{up,down}.sql
└── 0035_click_carries_its_network.{up,down}.sql
```

## Design decisions

These are the choices the tasks implement. Each names the alternative it
beat, because a decision without a rejected alternative is a preference.

### D-A. Configuration: a list keyed by driver name

Today `NetworkConfig` is five flat scalars — one driver, one account, one
credential reference, one source language, one rate limit — so a second
network has nowhere to go (`research.md` §3.2).

**Decision**: an explicit ordered list of driver keys, plus one block per
key:

```text
NETWORKS=awin,linkwise
NETWORK_AWIN_ACCOUNT_ID=...
NETWORK_AWIN_API_KEY=...
NETWORK_AWIN_SOURCE_LANGUAGE=de
NETWORK_LINKWISE_ACCOUNT_ID=...
NETWORK_LINKWISE_API_KEY=...
NETWORK_LINKWISE_SOURCE_LANGUAGE=el
```

The driver key is also what `network_account.credential_ref` names, which
is what finally gives that column a job: `credential_ref = 'awin'` resolves
to the `NETWORK_AWIN_*` block, in configuration, never in the database
(ADR-0003, FR-093).

**Why the list is explicit rather than inferred from which blocks are
present**: FR-091 requires that an incomplete network be reported *by name*
at ERROR rather than silently skipped. If presence implied intent, a typo in
`CASHBACK_NETWORK_LINKWISE_ACCOUNT` would make the network vanish rather
than complain. Naming it first makes a missing key a failure with a name in
it.

**Why the driver name is a safe key segment**: `validateNetworkDriver`
already refuses separators (`config/cashback.go:536-546`) — lowercase
letters, digits and underscores, starting with a letter — so
`NETWORK_<DRIVER>_` cannot be made ambiguous by a driver name.

**Why this is not the thing the current code refused.** `NetworkConfig`'s
own comment states the rejected design, and it is right to reject it:

> *'…there is one credential set rather than a per-network map: a second
> network is a second adapter and a **deliberate configuration change**, not
> something a **wildcard environment lookup** should be able to conjure.'*
> — `config/cashback.go:251-257`

An explicit `NETWORKS` list is precisely the deliberate change that comment
asks for, and precisely not the wildcard lookup it refuses. Nothing is
conjured by a key existing; a network runs because it was named.

**Rejected**: indexed keys (`..._1_DRIVER`) — the index is a second name for
a thing that already has one, and reordering the list silently rebinds
credentials. **Rejected**: one JSON blob — it puts a parser between an
operator and a typo, and hides which key was wrong.

**Old keys** (`NETWORK_DRIVER`, `NETWORK_ACCOUNT_ID`, `NETWORK_API_KEY`,
`NETWORK_API_SECRET`, `NETWORK_SOURCE_LANGUAGE`): refused at startup with a
message naming the old key and its replacement. A silent alias would let a
single-network deployment keep working while believing it had two.

### D-B. One registry, in the composition root

Two switches disagree today: `networkAdapter` (`cmd/apivo/networks.go:138`)
knows only the fixture, while `documentedNetwork`
(`cmd/apivo/connect_network.go:190`) knows the fixture *and* Awin. So a
driver can be seeded into `cashback.network` and then refuse to start.

**Decision**: one `map[config.NetworkDriver]registration` in a new
`cmd/apivo/registry.go`, where a registration carries both the driver's
documented facts and its constructor. Both call sites read it. A driver
absent from the map is absent from both, which is the property FR-092 asks
for.

**Why the composition root**: `internal/arch/network_isolation_test.go`
rule A forbids *any* domain package naming an adapter — that is what makes
the second adapter a new directory rather than a project. A registry under
`internal/cashback/networks/` would import `awin` and `linkwise` and fail
that test. It failing would be correct.

### D-C. Attribution: the click's network, enforced twice

`GetClickByRef` matches on `click_ref` alone
(`clickout/queries/click.sql:53-57`), and `ReportsAwaitingCredit` excludes
by `(network_id, external_id)` and never by `click_id`. The `entry` index on
`click_id` is an ordinary index, not unique
(`0013_cashback_earnings.up.sql:105`). So one click reference, if it
appeared on two networks, backs **two credits** — verified link by link
(`research.md` §4.1).

**Decision, in both layers, because they fail differently**:

- **Query** (FR-096): the lookup takes the reporting network and returns a
  click only if the click was issued through it. A reference that matches a
  click on another network is *unattributed*, and distinguishably so
  (FR-098) — "no such reference" and "that reference belongs to another
  network" are different operator actions.
- **Database** (FR-097): `entry_click_id_idx` becomes a **partial unique**
  index — at most one non-reversal entry per click — in migration 0033,
  with a test asserting the second insert is refused by SQLSTATE. This
  mirrors `entry_one_per_report` exactly, including the reversal exclusion
  0032 established for the same reason.

**Why both**: the query is what makes the system behave; the constraint is
what makes it *unable* to misbehave when a future query forgets. Principle
VIII says the database holds the rule.

### D-D. The click learns its own network (0035)

Reaching a click's network today is `click → offer → merchant_network →
network_id`: three joins for a fact every attribution decision needs, and
too expensive to put in a constraint at all.

**Decision**: `cashback.click` carries `network_id`, written at click-out
from the route the offer belongs to, with a **composite foreign key** back
to `(offer_id, network_id)` so the denormalised value cannot disagree with
the route it came from. That is the same technique `click_id_account_unique`
already uses to carry the ownership rule in a key rather than a trigger
(`0012:50-54`).

**Rejected**: a trigger — it validates on write and says nothing about rows
already there. **Rejected**: leaving it joined — then D-C's query predicate
is a three-table join on the hot attribution path, and FR-096 becomes a
performance argument instead of a rule.

### D-E. Arbitration: a published route must be usable (0034)

`merchant_network_one_preferred` already guarantees **at most one** preferred
route per retailer (`0011:181-183`). Two things it does not guarantee, and
neither matters until a retailer is on two networks:

- a preferred route may be `left_network` or `paused` — a dead route keeps
  publishing over a live one;
- a preferred route may be one that **cannot carry a click reference** —
  contract rule 10, and the failure where the member is never credited while
  every diagnostic reads healthy;
- there may be **zero** preferred routes — the retailer publishes nothing
  while a perfectly good route sits beside it.

**Decision**: 0034 constrains the published route to one that is alive *and*
attributable, and adds the demotion path — when a route leaves or its network goes inactive, its preference is
withdrawn and a surviving active route takes over. Which one is **a recorded
decision** (FR-099) with a named human and a non-blank reason where an
operator makes it (FR-101), and first-import as the initial value.

**Not decided here**: whether the automatic choice should follow the higher
member rate. That is **Q10**, founder-only, and its default is explicitly
*no automatic rate arbitration* — a catalogue that changes under members
because a rate moved is a product decision, not a schema one.

### D-F. Job names per network

`catalogue.ImportJobName` is a package constant
(`catalogue/schedule.go:34`), so two networks register the same job name and
the second registration is refused at startup — the deployment fails to
start rather than mis-polling, which is the good failure, but it is still a
failure.

**Decision**: derive it per publisher account exactly as
`networks.ForwardJobName` and `TrailingJobName` already do
(`sweeps.go:65-76`). This is a rename of an existing pattern onto the one
place that missed it, not a new mechanism. The capacity check then counts
what was actually registered (FR-095).

### D-G. The conformance suite runs over what ships

The suite runs against the fixture. A second real adapter that is never run
through it is an adapter held to nothing.

**Decision**: the suite becomes a table over the shipped registry (D-B), so
adding an adapter to the binary adds it to the suite. Rule 7 — every yielded
value has passed its own `Validate` — was asserted by nothing until
[#497](https://github.com/Nomos-N4s/apivo-news/pull/497); an adapter that
skipped `Validate` and yielded a mixed-currency report passed **eleven of
twelve** scenarios. That is closed before this feature begins precisely
because the suite is the thing a second adapter is held to.

### D-H. Adapters last, and only against recordings

Neither Awin nor Linkwise publishes a transactions schema. FR-105 is
therefore not a process nicety: it is the reason `*awin.Client` is honestly
incomplete rather than dishonestly finished.

**Decision**: an adapter phase for a network begins only when a **recorded,
redacted response** from a real publisher account is committed under that
adapter's `testdata/`. Until then the network is *configurable and
connectable* — its row, its credential reference, its catalogue path where
one exists — and its `FetchTransactions` does not exist. A deployment that
configures it gets a named refusal, not a silent zero.

## Phases

Phases 0–2 are unblocked and ordered by dependency. Phase 3 is per-network
and gated.

### Phase 0 — Safety, before anything can move money wrongly

Closes the two silent findings. It is first because every later phase makes
a second network more likely to exist, and these are the failures that do
not announce themselves.

- 0035 click carries its network; 0033 one click backs one credit; the
  network-predicated lookup; the unattributed reason that distinguishes
  "wrong network".
- 0034 a published route must be alive and attributable, and the demotion
  path.
- Each with a real-Postgres test asserting the **database** refuses.

### Phase A — The seam

- The registry (D-B), and the two switches deleted into it.
- Configuration as a list (D-A), with the old keys refused by name.
- The composition root connecting each configured network.
- Per-network job names (D-F) and the capacity arithmetic.

### Phase B — Operator surface and proof

- Per-network filtering on the operator queues; the connected-networks
  listing (FR-102, FR-103).
- The operator move-the-route endpoint (FR-101).
- The conformance suite over the shipped registry (D-G).
- The integration test that a two-network deployment polls both (SC-020).

### Phase C — Awin, completed *(gated on a recording)*

`FetchTransactions` and `Limits()`, mapped only from recorded responses.
This is 002's T139–T144, pulled into this feature because a second network
cannot prove a seam the first has never used (Q12's default).

### Phase D — Linkwise *(gated on Q11)*

The catalogue path first — it is the one capability Linkwise describes
publicly in concrete terms (XML/CSV feeds, publisher-configurable). Then
deeplinks, noting two facts Linkwise publishes about itself: a programme
carries **`allow_deeplinking`**, and the click tag has **exactly five subid
slots** (research.md §5.2). A programme that may not be deeplinked yields
transactions that are unattributed *by construction*, which the operator
queue must state rather than treat as a failure; and five slots is a budget
in which `IssuedClickRef` is the one entry that cannot be given up. Then
transactions, against a recording.

## Complexity Tracking

| Addition | Why it is necessary | Simpler alternative rejected |
|---|---|---|
| Four migrations for one feature | Each carries a different rule, and Principle VIII wants rules in the database. Bundling them would make a partial revert impossible | One migration — a revert of the arbitration rule would drag the attribution rule with it |
| `click.network_id` denormalised | The attribution predicate is on the hot path and wants to be a constraint | Keep the three-table join — makes FR-096 a performance argument rather than a rule |
| A registry in the composition root | Two switches already disagree, and the architecture test forbids the domain naming an adapter | A shared domain registry — fails `network_isolation_test.go`, correctly |
| Phase C completing Awin | A seam proved only by a fixture is not proved | Ship Linkwise alone — then one live adapter still does not exist |

## Next Steps

1. Founder: **Q10–Q13**. Only **Q11** blocks anything (Phase D). Q13 blocks
   Phase C/D adapter work only if a recording shows split currencies.
2. Phases 0 → A → B in order; they are the whole of the unblocked work and
   are what makes a third network cost one package (SC-024).
3. Phase C and D open when their recordings land, per FR-105.
