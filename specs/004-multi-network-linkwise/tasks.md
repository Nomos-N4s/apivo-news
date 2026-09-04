---

description: "Task list for many affiliate networks — Linkwise beside Awin"
---

# Tasks: Many Affiliate Networks — Linkwise Beside Awin

**Input**: Design documents from `/specs/004-multi-network-linkwise/`

**Prerequisites**: [plan.md](plan.md), [spec.md](spec.md), [research.md](research.md), [data-model.md](data-model.md), [contracts/](contracts/), constitution v1.2.0

**Numbering**: continues 002's high-water mark. 002 ends at T147; this
feature starts at **T200**, leaving room for 002's remaining Phase 10 work.

**Tests**: **Included and mandatory.** Every rule this feature moves into
the database carries a test asserting the *database* refuses, by SQLSTATE
and by constraint name (Principle VIII). Every adapter is held to the shared
conformance suite. The 90% Go coverage gate is unchanged.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: can run in parallel — different files, no dependency on an
  incomplete task
- **[Story]**: which user story (US1–US5 in [spec.md](spec.md))
- Every task names its file path

## Commit discipline

Per the repository's working practices: **one PR per task or per tight
pair**, and every intermediate commit builds and passes its own tests. A migration and the
test that proves the database refuses belong in the same commit; a
migration and the read-path change that uses it do not.

---

## Phase 0: Safety — before a second network can exist

**Purpose**: close the two findings that misbehave *silently*. First,
because every later phase makes a second network more likely, and these are
the failures that do not announce themselves. See
[research.md](research.md) §4.

**Migration numbers below are written `<n+1>`…`<n+4>`, not `0033`…`0036`.**
`<n>` is whatever `internal/platform/db/migrations/` ends at on `main` the
day the pull request opens; this feature reserves the range 0033–0038 and
fixes none of it here. `golang-migrate` records the highest version it
applies, so a migration landing out of order strands the ones below it
unrun — and these phases land out of numeric order on purpose. See
[plan.md](plan.md) § "Migration numbers are assigned when the PR opens".

- [ ] T200 [US2] Migration `<n+1>_click_backs_one_credit.{up,down}.sql` in `internal/platform/db/migrations/`: drop `entry_click_id_idx`, recreate it **unique** over `(click_id) where click_id is not null and reversal_of_id is null`, with the index comment stating why reversals are excluded
- [ ] T201 [US2] Test in `internal/cashback/earnings/` against real Postgres: two non-reversal credits citing one click through different `network_transaction_id` — assert SQLSTATE `23505` naming `entry_click_id_idx`; and assert a reversal citing the credit's own click is still permitted
- [ ] T202 [US2] Teach the crediting path to recognise that refusal **by name**, the way `earnings/open.go:200-201` already recognises `entry_one_per_report`, so a duplicate is an unattributed row rather than a 500
- [ ] T203 [US2] Migration `<n+3>_click_carries_its_network.{up,down}.sql`: `offer_id_merchant_network_unique`, `merchant_network_id_network_unique`, the two new `cashback.click` columns, the backfill, `set not null`, both composite foreign keys, and `click_network_id_idx`
- [ ] T204 [US2] Test against real Postgres: a click whose `network_id` names a network its route does not belong to is refused with SQLSTATE `23503` naming `click_network_matches_route`; and the backfill leaves no null on a seeded database
- [ ] T205 [US2] `internal/cashback/clickout/queries/click.sql`: `GetClickByRef` takes the reporting network and returns a row only when `click.network_id` matches; write `network_id` at click-out. Regenerate `internal/cashback/clickout/store` with sqlc
- [ ] T206 [US2] Attribution: a reference matching a click on **another** network yields no match and queues the report as unattributed with reason `foreign_network` — with a test that the *sibling* report on the issuing network still credits normally
- [ ] T207 [P] [US3] Migration `<n+2>_preferred_route_must_be_publishable.{up,down}.sql`: `merchant_network.can_attribute boolean not null default true`, and `merchant_network_preferred_is_publishable` check, both with comments
- [ ] T208 [US3] Test against real Postgres: preferring a `left_network` route and preferring an `active` route whose `can_attribute` is false are each refused with SQLSTATE `23514` naming the constraint (SC-022, SC-026)
- [ ] T209 [US3] The demotion path in `internal/cashback/catalogue/`: when a route becomes `paused`/`left_network`, or its network becomes inactive, its preference is withdrawn and a surviving publishable route takes over — recorded, with who and why
- [ ] T210 [P] [US3] Operator query: retailers that have publishable routes and publish nothing. This is the cross-row rule [data-model.md](data-model.md) deliberately does not make a constraint, and it must be visible rather than silent

- [ ] T254 [US2] **The click-out refuses a member who has not opted in** (FR-110), in `internal/cashback/clickout/`. Nothing checks it today, so a signed-in member who never accepted terms can click out and be credited. This lands **before** T255, or that constraint would refuse a member who had already bought something
- [ ] T255 [US2] Migration `<n+4>_entry_currency_is_the_members.{up,down}.sql`: `participation_account_currency_unique`, and `entry_currency_is_the_members` as a composite foreign key from `(account_id, currency)`. Plus `network_account.reports_currency`, nullable, with its format check
- [ ] T256 [US2] Test against real Postgres: an entry whose currency differs from the member's participation currency is refused with SQLSTATE `23503` naming the constraint; and a participating member's `default_currency` cannot be restated while an entry references the old one (SC-027)
- [ ] T257 [US2] The crediting path recognises that refusal by name and queues the report for an operator with the currency in it (FR-109), rather than failing the whole window
- [ ] T258 [US1] `connect-network` and the seed refuse a publisher account whose **declared** `reports_currency` is not the deployment's payout currency, naming both (FR-108). A null is reported, never refused — nobody has established it yet, and that is the state to make visible
- [x] T259 [P] [US3] **A slug can name a retailer in any script the deployment publishes in** (FR-111), in `internal/cashback/catalogue/transliterate.go` (a file of its own; `identity.go` gains one call). Greek follows **ELOT 743 / ISO 843**, the passport standard, which lands on the retailers' own Latin spellings — Plaisio, Skroutz, Germanos, Kotsovolos (SC-028)

  **Wider than the task as written, because the task was measured against
  `main` before it was implemented.** Greek was the known half. The other
  half is Latin characters NFD does not decompose — `ß ø æ œ ł đ þ ð` — and
  it is worse, because it produces a plausible wrong slug rather than an
  empty one, so `FallbackSlug` never fires and nothing looks broken:

  | name | `main` | now |
  |---|---|---|
  | `Καταστήματα` | `""` | `katastimata` |
  | `Weißenhaus` | `wei-enhaus` | `weissenhaus` |
  | `Straße` | `stra-e` | `strasse` |
  | `Ærø` | `r` | `aero` |

  `Ærø` → `r` is the one to look at twice: `merchant_slug_unique` is global,
  so a one-letter slug is a collision waiting for the next name that reduces
  the same way.

  **Transliteration runs BEFORE the fold**, not after. After would halve the
  table and would be wrong: Greek writes a diaeresis precisely to say two
  vowels are *not* a digraph, and the fold strips it, so `Προϋπολογισμός`
  would read as the `ου` digraph. Mutation-checked.

  **Two ELOT 743 rules are deliberately absent**: `μπ → b` and `ντ → d` apply
  only word-initially. A slug is permanent, so a rule that is occasionally
  wrong is worse than one that is plainly literal; `μπ` stays `mp`.

  **Consequence to know about.** This is *not* output-identical for
  everything that already slugs — `Weißenhaus` moves, and that is the point.
  It *is* byte-identical for every name built from ASCII plus a decomposable
  accent, verified by diffing the corpus against output captured from
  `origin/main`, and frozen by `TestTheSlugsThatMustNotMove`. Slugs are only
  computed for routes never reported before, so a merchant imported *before*
  this keeps its old slug and the same merchant arriving later from a second
  network would get the new one — two merchant rows. Today that is
  theoretical: nothing but fixture data has been imported. It stops being
  theoretical at the first real `FetchCatalogue` run, which is why T259 is a
  gate and not a preference.
- [ ] T260 [US3] Cross-network unification stops resting on a name coincidence alone (FR-112): an operator declares two routes one retailer and can undo it, recorded, in `internal/cashback/catalogue/`. The importer's own comment already says two programmes with one name may be two businesses

**Checkpoint**: one click cannot back two credits; a dead or unattributable route cannot publish; a foreign-network reference is a named outcome rather than a mystery.

---

## Phase A: The seam

**Purpose**: make a second network *configurable and connectable*. Nothing here needs either network's wire format.

- [ ] T211 [US1] `cmd/apivo/registry.go`: one `map[config.NetworkDriver]registration` carrying each shipped driver's documented facts and its constructor. In the composition root, never in the domain — `internal/arch/network_isolation_test.go` rule A forbids a domain package naming an adapter
- [ ] T212 [US1] Delete the `networkAdapter` switch (`cmd/apivo/networks.go:138`) into the registry
- [ ] T213 [US1] Delete the `documentedNetwork` switch (`cmd/apivo/connect_network.go:190`) into the registry
- [ ] T214 [US1] Test in `cmd/apivo/`: every registry entry is both **servable and seedable**, and a driver absent from the registry is absent from both. This is the defect FR-092 names — a driver that can be seeded into `cashback.network` and then refuses to start
- [x] T215 [US1] `internal/platform/config/networks.go` (a file of its own, not `cashback.go`): `NETWORKS` as an ordered list plus one `NETWORK_<DRIVER>_*` block per entry, per [contracts/config.md](contracts/config.md). `NetworkConfig` becomes a slice; each element keeps `Secret` for both credential halves. `network_account.credential_ref` becomes per network with it — it was the literal `NETWORK_API_KEY`, true while one network existed and a lie the moment the keys grew a driver in them
- [x] T216 [US1] Config validation: a duplicate entry and a malformed driver name are each refused at startup. **A driver not in the registry is not refused here**: config checks the SHAPE of a name and defers membership to `cmd/apivo/registry.go`, which is where the adapters are. Refusing it in config would put the list of shipped networks in two places, which is the disagreement the registry (T211) was built to end — and `lookupNetwork` already refuses an unknown driver by name, listing what the binary ships
- [x] T217 [US1] Config validation: a network with a missing key is reported at ERROR **by name** — `"linkwise" cannot poll: NETWORK_LINKWISE_ACCOUNT_ID, NETWORK_LINKWISE_API_KEY are unset`. **It stops cashback**, which FR-091 as written says it must not — see the divergence note below
- [x] T218 [US1] Config: the five old flat keys are **refused**, not aliased, each with a message naming its replacement
- [x] T219 [US1] Test: a block present for a driver absent from `NETWORKS` is ignored; zero networks is not a startup failure and mounts cashback normally

**FR-091 divergence, founder decision of 2026-09-04.** The contract
([contracts/config.md](contracts/config.md) rows 2-3) and FR-091 say an
incomplete network must not stop *the deployment or the networks beside it*,
and that the others keep polling. The code says an incomplete network stops
**cashback** — all of it, however many other networks are fine — and leaves
the process serving the news site.

The deciding fact, found while implementing: `requireCashbackComplete`
returns an error from config parsing, so putting these keys in `Missing()`
kills the **process**. A typo in `NETWORK_LINKWISE_API_KEY` would have taken
`apivo.news` down, and that risk grows with every network added. The founder
was offered "start and poll the ones that work" and rejected it: cashback
moves members' money, and a network that cannot poll is one whose
transactions never arrive — members would click, buy, and never be credited,
with nothing failing anywhere.

So there are three states, not two, and the spec only had two:

| | process | cashback | polling |
|---|---|---|---|
| a network key is missing | serves | **not mounted** | none |
| `NETWORKS` empty | serves | mounted | none, said at ERROR |
| no `network_account` row | serves | mounted | none, said at ERROR |

Recorded rather than resolved: the founder amends the spec separately, and
this note exists so the divergence does not go silent. Issue: #513.
- [ ] T220 [US1] `cmd/apivo/cashback.go`: connect every configured network, resolving each adapter's credential through `network_account.credential_ref` (FR-093), and wire one set of sweeps per publisher account
- [ ] T221 [US1] `internal/cashback/catalogue/schedule.go`: `ImportJobName` becomes a per-account derivation, exactly as `networks.ForwardJobName` and `TrailingJobName` already are (`sweeps.go:57-76`)
- [ ] T222 [US1] Update the capacity arithmetic and its comment in `cmd/apivo/main.go:440-446`: three jobs per network, floor 14 → 20 → 26, asserted by `locker.CheckCapacity` against the jobs actually registered (FR-095)
- [ ] T223 [US1] `cmd/apivo/networks_wiring_test.go`: two configured networks register **six** distinct sweep jobs plus two distinct catalogue imports, under distinct lock names, and the capacity check refuses a pool too small for them
- [ ] T224 [US5] `internal/cashback/networks/fixture2/`: a second fixture adapter, network id `fixture2`, so a two-network deployment runs locally with **no credentials**. The fixture's own id is a package constant on purpose — *"a deployment that could rename it would have rows attributed to a network nobody can find the code for"* (`fixture/fixture.go:21-22`) — so a second local network is a second package, which is exactly what SC-024 claims is cheap. This task is that claim, executed: one directory, one registry entry, nothing else

**Checkpoint**: a two-network deployment starts, connects both, and registers distinct jobs under distinct locks.

---

## Phase G: The reporting horizon

**Purpose**: stop the forward cursor advancing past ground the network has
not reported. No vendor dependency, so it lands beside Phase A.

The forward sweep asks for a window ending **now**. A network that reports
late answers that window cleanly and emptily for the part it has not reached,
and the cursor advances anyway — nothing is lost, because `nextTrailingWindow`
walks forward and re-reads every period exactly once, but it does so at
roughly **+100 days** (`DefaultTrailingLag`). A member waits a quarter for a
credit earned today, and no error stream shows it. It becomes real loss only
where the network also refuses a window that old.

Rejected: yielding `AbandonedIteration(ErrNetworkUnavailable)` past a horizon.
It works, but it pays for correctness with the one diagnostic asset the money
path has — an adapter that reports "unavailable" on most ticks by design makes
rule 9's classification mean nothing, and the day the network is genuinely
down is indistinguishable from the steady state. Also rejected: overlapping
windows, which are safe but re-read a day of ground 96 times a day against a
rate limit measured in requests per minute.

- [x] T290 [US1] `Limits.ReportingLag` in `internal/cashback/networks/window.go`: how far behind the present a network's answers are. **Zero is the ordinary value here, not the dangerous one** — unlike the two bounds beside it, which refuse zero because an unset `Limits` makes every window too wide. Negative is refused: a network cannot report the future
- [x] T291 [US1] `nextForwardWindow` ends the window at `now - ReportingLag` rather than at `now`, in `internal/cashback/networks/poll.go`. A lag of zero leaves the arithmetic exactly as it was, which is what every adapter declared before the field existed. Table tests in `poll_internal_test.go`
- [x] T292 [US1] `cashback.network.reporting_lag_minutes`, `networks.Documented` carrying it, and `connect-network` seeding it — so the value is an operator's row rather than an adapter's constant, the same way `max_query_window_days` and `rate_limit_per_minute` already are. **Written, not yet read back**: `Limits()` still comes from the adapter's own declaration, exactly as the two columns beside it do. Closing that is T261, and it closes all three at once
- [ ] T293 [US1] `GET …/ops/networks` renders the lag and the effective forward horizon beside the cursors, so **"deliberately behind" is visibly different from "stuck"**. Silence gains a new cause with T290, and an account whose `backfill_from` sits inside the lag reads nothing until the clock catches up. Depends on T228
- [ ] T294 [US1] Startup check: a configured lag where `lag + ForwardInterval` exceeds SC-001's 24h credit budget is reported at ERROR naming both numbers. A lag is a deliberate delay, and one large enough to break the promise should be a decision somebody made rather than a number nobody read

**Checkpoint**: a network that reports late is read only as far as it has
reported, and the delay is visible rather than inferred.

---

## Phase B: Operator surface and proof

- [ ] T225 [P] [US2] The unattributed reason discriminates four causes — `no_reference`, `unknown_reference`, `foreign_network`, `route_cannot_attribute` — in the store and in the API (FR-098, [contracts/http-api.md](contracts/http-api.md) §1)
- [ ] T226 [P] [US4] `GET …/ops/unattributed` and `GET …/ops/held` take `?network=`, name the network on every row, and answer **400** for a network that is not connected
- [ ] T227 [P] [US4] A network column in `GET …/ops/exports/ledger` and `…/exports/reconciliation`, JSON and CSV alike — added, never replacing an existing column
- [ ] T228 [US4] `GET /api/v1/cashback/ops/networks`: connected networks, each account's cursors, last poll and outcome, `driver_shipped`, and `credential_present` as a **boolean** — never the credential, never a prefix, never its length (ADR-0003)
- [ ] T229 [US3] `POST /api/v1/cashback/ops/merchants/{id}/route`: move the published route with the operator from the auth gate and a non-blank reason; **409** when the database refuses the target as unpublishable, **404** when there is no such route
- [ ] T230 [US4] Extend the OpenAPI document with the new endpoints and re-run the route-coverage and C-6 money guards (`cmd/apivo/openapi_routes_test.go`, `openapi_money_test.go`)
- [ ] T231 [US5] `internal/cashback/networks/conformance_test.go`: the suite becomes a table over the **shipped registry**, so an adapter compiled into the binary is an adapter the suite runs ([contracts/ports.md](contracts/ports.md) §4)
- [ ] T232 [US5] Conformance scenario for rule 3's clarification: two adapters run concurrently, and a `429` against one does not delay the other (FR-106)
- [ ] T261 [US4] `Limits()` reads the network's row, per network (FR-113) — or the port's claim that it does comes out. `cashback.network` carries both numbers per network already, and `connect.go:202` discards the row it reads
- [ ] T262 [US4] `POST …/ops/reconciliation/runs` takes the network and the publisher's own account identifier instead of a raw `network_account_id` UUID, so reconciling the second account does not need a `psql` session to find one
- [ ] T233 [US5] Conformance scenario for rule 11: an adapter yields a route it knows cannot carry a click reference, with the fact set rather than the route omitted
- [ ] T234 [US1] Integration test against real Postgres and the real scheduler: a two-network deployment polls both, imports both catalogues, and serves click-outs for both (SC-020)
- [ ] T235 [US5] Re-run `internal/arch/network_isolation_test.go` **unmodified** with a second adapter package present — SC-008 with two packages, and the proof that a third costs one directory (SC-024)

**Checkpoint**: two networks are visible, filterable and operable; the suite holds every adapter that ships.

---

## Phase C: Awin, completed — **gated on a recording**

**Gate**: no task here starts before T236 lands. FR-105 admits no exception,
and this gate is the reason `*awin.Client` is honestly incomplete today
rather than dishonestly finished ([research.md](research.md) §1).

- [ ] T236 [US1] Record and **redact** real Awin transaction responses from a live publisher account into `internal/cashback/networks/awin/testdata/`, to the standard `002/research.md` §9.2 set: at least one of each status the account has produced, one empty window, one paginated window, and one error
- [ ] T237 [US1] `awin.Client.Limits()` — the documented query window and rate, from the recording and the network's own published limits
- [ ] T238 [US1] `awin.Client.FetchTransactions` — resumable iteration per rule 4, verbatim payload per rule 1, `AbandonedIteration` per rule 8, every field mapped from the recording
- [ ] T239 [US1] Awin status mapping, **total** per rule 2: an unrecognised word is `ErrUnmappableStatus`, never a silent default
- [ ] T240 [US1] Register `awin` in the driver registry; the conformance suite runs it green against its recordings
- [ ] T241 [US1] Awin `can_attribute` (rule 11): Awin's `click_ref_param` is a network-wide fact, so the adapter declares routes attributable network-wide and says so once

**Checkpoint**: the first *live* network adapter exists, and the seam has been used by something real.

---

## Phase D: Linkwise — **gated on Q11**

**Gate**: Q11 (is there an approved publisher account, are credentials held,
is there a sandbox) is founder-only and unanswered.
[research.md](research.md) §5.2 lists what is unknown; **none of it may be
invented**. If Q11's answer is "no account yet", this phase does not start
and Phases 0–C are still the whole of the value.

- [ ] T242 [US1] Record and redact real Linkwise responses into `internal/cashback/networks/linkwise/testdata/`, same standard as T236. **This task is the gate**: it either produces recordings or it reports that it cannot, and nothing below starts on the second outcome
- [ ] T243 [US1] `internal/cashback/networks/linkwise/`: package skeleton — `ID`, `Account`, `Limits` from the recording, its own rate limiter and retry budget (FR-106)
- [ ] T244 [US1] Linkwise catalogue: `FetchCatalogue` over the publisher feed. The one capability public sources describe concretely — XML or CSV, publisher-configurable ([research.md](research.md) §5.1)
- [ ] T245 [US1] Linkwise `can_attribute` **per programme** (rule 11), from the `allow_deeplinking` flag Linkwise publishes on each programme. Where the adapter cannot determine it, it declares the network unattributable rather than defaulting to attributable
- [ ] T246 [US1] `linkwise.BuildDeeplink`: the click reference in Linkwise's own parameter, wrapping `ErrDeeplinkNotFormed` on any refusal (rule 5). Assert `IssuedClickRef` survives the round trip — ≥22 URL-safe characters, byte for byte
- [ ] T247 [US1] `linkwise.FetchTransactions`, every field from T242's recording. **If sale and commission arrive in different currencies, stop**: that is Q13, and `Reported.Validate` refuses it at the port by design
- [ ] T248 [US1] Register `linkwise` in the driver registry; the conformance suite runs it green
- [ ] T249 [US3] Seed a retailer live on both networks in `cmd/apivo/seed_cashback.go` and prove US3 end to end: one published route, the other demoted, and the takeover when the published one dies

**Checkpoint**: two real networks, both conformant, one published route per retailer.

---

## Phase E: Documentation and corrections

- [ ] T250 [P] `docs/ENVIRONMENTS.md` and `.env.example`: `NETWORKS`, the per-driver blocks, the retired flat keys and the `pool_max_conns` floor per network count
- [ ] T251 [P] `specs/004-multi-network-linkwise/quickstart.md` verified end to end against a clean checkout — two networks, both fixtures, no credentials
- [ ] T252 [P] A runbook section on connecting a network: seed the row, name it in `NETWORKS`, set its block, raise the pool, watch `GET …/ops/networks`
- [ ] T253 [P] Correct the seven documents [research.md](research.md) §6 lists as contradicted — including `002/contracts/ports.md`'s claim that the suite asserts rule 7, its description of a live contract test that does not exist, and the port doc comment saying the SC-008 isolation rule "is not written yet" when `internal/arch/network_isolation_test.go` exists

---

## Dependencies

```text
Phase 0 ──> Phase A ──> Phase B ──> Phase C (gated on T236)
                                └─> Phase D (gated on Q11 and T242)
Phase E follows whatever has landed.
```

- **T200–T210** depend on nothing and unblock everything. T203 (0035) must precede T205 (the query that uses `click.network_id`).
- **T254 must precede T255.** The currency constraint reaches the member's currency through their participation, so it makes crediting require an opt-in; without the click-out gate in front of it, the refusal lands on somebody who has already bought something.
- **T259 must precede T248** (the retailer live on both networks), and precede any Linkwise catalogue import that would otherwise write fallback slugs nothing can later match.
- **T211–T214** (the registry) must precede T215–T220 (configuration reading it) and T231 (the suite reading it).
- **T231** must precede T240 and T248, or an adapter ships unmeasured.
- **T224** (the second fixture) must precede **T234** (two networks polling) and **T235** (the isolation test with a second adapter package present) — without it neither can be run without credentials.
- **T236** gates all of Phase C. **T242** gates all of Phase D.
- Nothing in Phases C or D blocks anything in 0, A, B or E.

## Parallel opportunities

`[P]` tasks touch disjoint files. The largest genuinely parallel groups:

- **T207 + T210** with **T200–T206** — arbitration and attribution are different tables
- **T225, T226, T227** — three handlers, three files
- **T250, T251, T252, T253** — documentation, all independent

## What is deliberately not here

- **Frontend.** The operator surfaces are HTTP endpoints; their pages belong to the frontend workstream.
- **Automatic rate arbitration.** Q10's default is no.
- **A third network.** SC-024 asks that the third be cheap, not that it be built.
- **Cross-network deduplication.** One click backs one credit; two networks reporting one purchase resolves to one match and one `foreign_network` row, which is correct.
