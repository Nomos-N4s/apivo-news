# Research: Many Affiliate Networks, and Linkwise as the Second

**Feature**: 003-multi-network-linkwise
**Created**: 2026-09-03
**Status**: Draft

This document records what was *found*, with evidence, before anything was
decided. [spec.md](spec.md) states the requirements; [plan.md](plan.md)
states the approach. Where a finding contradicts a document already in the
repository, the document is named so it can be corrected rather than quietly
worked around.

---

## 1. The finding that reframes the feature

**The cashback product does not integrate Awin today.** It integrates the
fixture adapter, and nothing else.

`*awin.Client` does not satisfy `networks.Network`. Asserted at the
compiler, not inferred:

```
internal/cashback/networks/awin/zz_portcheck.go:5:26:
  cannot use (*Client)(nil) (value of type *Client) as networks.Network
  value in variable declaration: *Client does not implement
  networks.Network (missing method FetchTransactions)
```

`Limits()` is absent too. The non-test methods on `*awin.Client` are
`FetchCatalogue`, `ID`, `Account`, `RateLimit`, `Get`, `once`, `refusal`
and `BuildDeeplink` — the transport, the catalogue read and the deeplink
builder. The transactions half, which is the money, was never written.

The consequence reaches the binary:

- `networkAdapter` (`cmd/apivo/networks.go:138-147`) ships **only**
  `fixture`. `NETWORK_DRIVER=awin` returns a non-`ingestionOff` error,
  which `serve` treats as fatal: the deployment does not start.
- `documentedNetwork` (`cmd/apivo/connect_network.go:190-203`) *does* know
  `awin`. So `connect-network` will happily write both rows for a network
  the server then refuses to boot against. **Two driver registries have
  drifted apart**, and the drift is invisible until a deployment tries it.
- The shared conformance suite's adapter table
  (`conformance_test.go:96-98`) contains the fixture and nothing else. No
  real adapter has ever been held to the port contract.

This is deliberate, not rot. `specs/002-apivo-cashback-alpha/research.md`
§9.2 records that Awin publishes no schema for its transactions response —
the documented endpoint answers `{"properties": [], "examples": []}` — and
refuses to map money fields from guesswork until a real recorded response
exists. Tasks **T139** (transactions), **T140** (status mapping), **T142**
(recorded testdata), **T143** (wiring) and **T144** (conformance) are open
for exactly this reason.

**So this feature is not "add a second network to a working first one".**
It is *"make the first live adapter reachable, prove the seam with a second,
and let the evidentiary standard that blocked Awin apply to both."* Any plan
that assumes Awin polls in production is wrong.

---

## 2. What is already multi-network, and is not in scope to change

The repository splits cleanly at the composition root. Below it, almost
everything was built for many networks and is proved so.

### 2.1 The schema

`cashback.network` is a text-keyed table with a format check and **no enum
and no CHECK enumerating known networks**
(`0011_cashback_catalogue.up.sql:38-55`). No migration inserts a row into
it. A second network is *data*, written by `EnsureNetwork` from an operator
command — **not DDL**.

| Table | Scoping | Evidence |
|---|---|---|
| `network_account` | `unique (network_id, external_publisher_id)`, own cursors, own `backfill_from` | `0011:64-85`, `0023:27-44` |
| `merchant_network` | `unique (network_id, external_merchant_id)` **and** `unique (merchant_id, network_id)` | `0011:155,158` |
| `offer` | hangs off `merchant_network_id`, so the network follows from the band | `0011:223` |
| `network_transaction` | `unique (network_id, external_id, content_digest)`; one-root index on `(network_id, external_id)`; a BEFORE INSERT trigger refuses a supersede link that crosses networks | `0012:128-129,162-164,201-213` |
| `reconciliation_run` / `_difference` | keyed per publisher account throughout | `0015:22,33,59-60,125-130`, `0028:32-34` |
| `network.rate_limit_per_minute`, `max_query_window_days` | per-network columns | `0011`, renamed in `0026` |

`0011`'s own header says it in capitals: **"THE SAME RETAILER IS REACHABLE
THROUGH SEVERAL NETWORKS AT ONCE."** A database test already proves it
(`cashback_catalogue_test.go:457-478` inserts a second network and a second
route for one merchant).

`network_account.credential_ref` is documented as *"A KEY INTO
CONFIGURATION naming where this account's credential lives — never the
credential itself (ADR-0003)"* (`0011:89-90`). It is written and stored, and
**nothing resolves it yet**. It is the hook this feature finally uses.

### 2.2 The port

`networks.Network` has six methods and needs no new one. Its identity is
`Account() PublisherAccount`, **not** `ID() NetworkID` — the doc says so
explicitly: the id "is not an identity", and two adapters at one network
return the same id. Multi-network is therefore structurally the same problem
the port already solved: N adapters keyed by account.

### 2.3 The pieces that already scale

- `clickout.NewDeeplinks` is **already a variadic registry keyed by
  `NetworkID` that refuses two adapters claiming one network**
  (`deeplinks.go:47,74-79`). `main.go` feeds it at most one.
- Sweep job and lock names are already per account:
  `"network-poll:" + network + ":" + externalID` (`sweeps.go:65-76`).
- The catalogue's departure sweep is network-scoped:
  `MarkRoutesNotSeen … where network_id = $1` (`import.sql:150-155`). Two
  imports do **not** mark each other's retailers departed.
- `ValidateDeeplinkInputs` refuses `target.NetworkID != id`
  (`deeplink.go:119-141`) — the check that stops an Awin offer being routed
  through Linkwise. It becomes load-bearing for the first time here.
- `internal/arch/network_isolation_test.go` enforces SC-008 structurally and
  **already requires at least two adapter packages**, failing otherwise
  because "with fewer than two every rule here passes vacuously" (`:266`).

---

## 3. What breaks outright with a second network

Severity 1 — the process does not start.

### 3.1 The catalogue import's job name is a constant

```go
// internal/cashback/catalogue/schedule.go:34
ImportJobName = "cashback-catalogue-import"
```

It names both the scheduler job and its fleet-wide advisory lock. A second
`Imports.Register` hits the scheduler's duplicate-name refusal and `serve`
returns the error. Because the name is also the lock name, two imports
would additionally serialise against each other even if registration
succeeded. `sweeps.go` shows the intended shape: derive it per network.

### 3.2 Configuration has nowhere to put a second network

`config.NetworkConfig` is five flat scalars — `NETWORK_DRIVER`,
`NETWORK_ACCOUNT_ID`, `NETWORK_API_KEY`, `NETWORK_API_SECRET`,
`NETWORK_SOURCE_LANGUAGE` (`cashback.go:251-290`) — read by one `getenv`
each (`:383-392`). `validateNetworkDriver` refuses any separator a list
would need (`:536-546`), so `NETWORK_DRIVER="awin,linkwise"` fails loudly
today, which is the correct failure.

`getenv` is injected as `func(string) string`, so a wildcard scan over
`NETWORK_*_DRIVER` is impossible through that seam. **A driver-list key
plus per-driver suffixed keys is the only shape this signature supports**
without changing `FromEnv`'s contract.

**And the credential seam does not exist at all.** `NETWORK_API_KEY` and
`NETWORK_API_SECRET` are parsed and logged set/unset (`:348-349`) and **no
production code path ever consumes them** — no adapter anywhere is
constructed with a credential. This is new design, not a generalisation.

### 3.3 The composition root threads exactly one adapter

`serve` holds one `adapter`/`connected`/`networkOff` triple
(`main.go:196-210`) and threads it into four consumers. The signatures to
widen: `newAuthenticatedRoutes` (`main.go:529`), `newClickOuts`
(`main.go:868`), `newCatalogueImport` (`catalogue.go:34`),
`newNetworkSweeps` (`networks.go:106`).

The pairing invariant that `connectNetwork`'s doc comment protects — the
poller and the click-out must not disagree about which network is connected
— survives only if the **same collection** feeds both.

Capacity arithmetic follows: three jobs per connected network at two
connections each, against a `registered += 2` hardcoded in `serve`.

---

## 4. What misbehaves *silently* — the money-safety findings

Severity 0. These do not fail; they pay the wrong person, or nobody.

### 4.1 One click can back TWO credits

Verified link by link, each independently:

1. **The click carries no network.** `cashback.click` has no network column,
   and `click_ref` is **globally** unique, not unique per network
   (`0012:26-55`). The issuing network is derivable only by joining
   `offer → merchant_network → network_id`.
2. **The matcher ignores the network.** `GetClickByRef` is
   `where click_ref = $1` (`clickout/queries/click.sql:53-56`), and
   `earnings.Report` carries no network field (`attribution.go`).
3. **The caller holds the network and throws it away.**
   `ReportsAwaitingCredit` selects `nt.network_id` (`lifecycle.sql:26`) and
   `lifecycle.go:275` passes only the reference.
4. **Nothing excludes a second credit on one click.**
   `ReportsAwaitingCredit`'s already-credited test is scoped to the
   transaction chain, `where cited.network_id = nt.network_id and
   cited.external_id = nt.external_id` (`lifecycle.sql:36-41`) — never by
   `click_id`. And `entry` carries only an **index** on `click_id`, not a
   unique constraint (`0013:105`).

So a report from network B whose reference names a click issued through
network A is matched, is not excluded, and **opens a second entry for one
purchase** — priced from network A's rate snapshot and billed to network A's
`brand_id` (`BrandOfOffer`, `lifecycle.sql:52-55`), while resting on network
B's commission. `entry_guard` freezes both after insert, so the wrong credit
can only be reversed, never corrected.

Today this is unreachable: only one network is live, and a foreign 128-bit
reference will never be found in `cashback.click`. It becomes reachable the
moment a second network is fed Apivo's own minted references.

The mirror image also holds: a report whose reference matches a click on
*another* network is not queued as unattributed either.

### 4.2 The preferred route is written once and never again

`merchant_network.preferred` is the schema's whole arbitration rule — the
catalogue publishes bands only from the preferred, active route on an active
network (`catalogue/queries/detail.sql:76-85`), and a partial unique index
caps it at one per merchant (`0011:181-183`). Zero preferred routes is legal
and publishes nothing.

**No SQL anywhere sets `preferred` after insert.** `UpsertRoute`'s
`ON CONFLICT` sets only `retrieved_at, raw_payload, status`
(`import.sql:180-184`); `import.go:188-203` passes it on insert alone.

Two consequences, both concrete:

- The **first network to import a retailer owns its member-facing rate
  forever**, whatever the other network pays.
- A preferred route that later goes `left_network` is **never demoted**, so
  a retailer that leaves the preferred network publishes nothing while still
  live on the other — a merchant page with zero bands and nothing logged.

### 4.3 Currency: the sharpest external constraint

`Reported.Validate` requires `SaleAmount` and `Commission` to carry the
**same** currency, because the evidence row stores one currency column for
both (`reported.go:217-220`). A network reporting the sale in the
advertiser's currency and the commission in the publisher's is refused at
the port — a migration, not an adapter tweak. This must be established
against a real recorded response before any field is mapped.

### 4.4 The conformance suite did not assert contract rule 7

Rule 7 says every yielded value has passed its own `Validate`, and both the
port's doc comment and `contracts/ports.md:244` say **the conformance suite
asserts it**. It did not: the only `Validate` call was on `Limits`.

Demonstrated: an adapter that skips `Validate` and yields a mixed-currency
report passed **eleven of the twelve** scenarios. Filed as
[#496](https://github.com/Nomos-N4s/apivo-news/issues/496) and closed by
[#497](https://github.com/Nomos-N4s/apivo-news/pull/497) before this feature
begins, because the suite is what a second adapter is held to.

---

## 5. Linkwise

### 5.1 What is established

Linkwise (`linkwi.se`) is an affiliate network founded in **2008**, based in
Athens, describing itself as the first and largest in **Greece** and as
operating across **SE Europe**. It reports roughly **500 advertisers** and
**15,000 publishers**, across fashion, retail, travel, services, banking and
insurance. It has run its **own custom-built platform since 2012** — so it
is not a white-label of a network we already understand, and nothing about
Awin's shapes transfers to it by assumption.

Two capabilities are described publicly in terms concrete enough to plan
around:

- **Product feeds.** Advertisers publish XML feeds with standardised tags,
  and a publisher can additionally *build its own* feed, in **XML or CSV**,
  choosing which fields it carries and which categories and programmes it
  covers. That is a catalogue path, and a publisher-configurable one.
- **SubID tracking**, described as available *"when a network/program
  supports it"* — that is, **per programme, not guaranteed network-wide**.

### 5.2 What is not established, and could not be from here

**Linkwise publishes no public API documentation.** Two search sweeps and
three attempted fetches produced no endpoint, no hostname, no authentication
scheme and no field name. Every trail ends at the same instruction: sign in
to the publisher dashboard, or ask an account manager. A third-party
click-tracking vendor advertises a Linkwise connector as *"API and/or
Postback"* — evidence that **some** machine interface exists, but its own
page is unreachable from this environment and the wording establishes
neither which, nor what it returns.

So all of the following are **unknown**, and none may be invented:

| Unknown | Why it matters |
|---|---|
| Transactions endpoint, auth, pagination, cursor semantics | `FetchTransactions` cannot be written |
| Field names and types for sale amount and commission | C-6; `Reported.Validate` |
| Whether sale and commission share a currency | §4.3 — a migration, not an adapter tweak, if they do not |
| Status vocabulary and its mapping to `pending/confirmed/rejected` | the crediting lifecycle |
| Whether the click reference survives to the transaction, and under what parameter name | C-2; the whole attribution path |
| Documented rate limits | `Limits()`, and the fleet-wide poller |
| Whether a sandbox exists | whether anything can be recorded before an account is live |

### 5.3 The constraint this puts on the plan

This is **the same position Awin was in**, and 002 answered it the right
way: `002/research.md` §9.2 refused to map a money field from documentation,
and the result is an Awin adapter that does catalogue and deeplinks and
deliberately does not do transactions. That refusal is why `*awin.Client`
does not satisfy the port (§1) — the gap is a *recorded* decision, not an
oversight.

The plan therefore may not contain a task that says "implement the Linkwise
transactions call". It contains, instead:

1. Work that is **knowable now** — everything in §§2–4: the registry, the
   per-network job names, the attribution constraint, the arbitration of a
   retailer on two networks. None of it needs a Linkwise endpoint, and all
   of it is what makes the second network cheap when its shapes arrive.
2. A **recording step** gated on Q11, which produces redacted responses
   under the adapter's own `testdata/`.
3. Adapter work that begins **only** once (2) has produced a recording,
   held to FR-105.

**The one thing already decidable about Linkwise** is that SubID support is
per-programme. If a programme carries no SubID, a click through it cannot
carry an `IssuedClickRef`, and every transaction from it arrives
unattributed **by construction** rather than by failure. That is not a bug
to fix in the adapter; it is a fact the operator queue must be able to state
(FR-098), and a reason a programme might not be worth publishing at all.

### 5.4 Currency

Greece and Cyprus are euro. Linkwise's stated SE-European reach includes
markets that are **not** — so "Linkwise is EUR" is an assumption about the
programmes we would join, not a fact about the network. It is recorded here
as an assumption, and FR-105's recording is what would settle it.

---

## 6. Documents this research contradicts

Each should be corrected rather than worked around:

| Document | Says | Actually |
|---|---|---|
| `002/data-model.md:58` | `rate_limit_per_second int not null` | renamed to `rate_limit_per_minute` in `0026` |
| `002/contracts/ports.md:244` | the suite asserts rule 7 | it did not, until #497 |
| `002/contracts/ports.md` §2 | "rate-limit adherence under concurrency" | the suite's rate scenario is sequential |
| `002/contracts/ports.md` §2 | a live contract test, skipped without credentials | no such test exists |
| `networks/network.go:169-220` | nine numbered contract rules | `ports.md` numbers ten; the tenth is unnumbered prose at `:162-167` |
| `002/tasks.md` | Phases 1-6 largely unticked | most are implemented; the file is stale |
