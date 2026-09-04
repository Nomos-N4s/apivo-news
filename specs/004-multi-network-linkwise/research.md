# Research: Many Affiliate Networks, and Linkwise as the Second

**Feature**: 004-multi-network-linkwise
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

### 4.5 The one thing that unifies a retailer across networks cannot name a Greek one

`catalogue/import.go:246-260` (`merchantForSlug`) is the **only** mechanism
that recognises a retailer already imported from another network: it matches
on `Slug(name)`. A name that produces no slug falls to
`FallbackSlug(network, externalID)` (`identity.go:104`), which by
construction contains **the network's own id** — so it can never equal the
other network's slug for the same shop.

`Slug` keeps a rune only when `r < unicode.MaxASCII && (IsLetter || IsDigit)`
(`identity.go:66`). Greek letters are all above `MaxASCII`, and the fold that
runs first strips Latin diacritics rather than transliterating. Run against
real Greek retailer names:

| Programme name | `Slug(name)` |
|---|---|
| `Πλαίσιο` | `""` |
| `Κωτσόβολος` | `""` |
| `Γερμανός` | `""` |
| `Σκρουτζ Α.Ε.` | `""` |
| `Public` | `public` |
| `Zara` | `zara` |
| `e-shop.gr` | `e-shop-gr` |

**Every Greek-named retailer slugs to the empty string.** So the headline use
case of this feature — one retailer, two networks, the better route
published (US3) — is structurally impossible for exactly the network being
added, and it fails silently: two `merchant` rows, two catalogue entries, two
different rates, and nothing anywhere saying they are one shop.

It is worse than a missed match. `FallbackSlug` embeds the network id, so a
member-facing identifier for a Greek retailer would read as the network's
internal reference rather than as a shop.

The slug is also doing a second job here it was never asked to do: it is
*identity*, not just a URL segment. A retailer's identity across networks
cannot rest on how its name transliterates.

### 4.6 A second currency is a trapped balance, not a rejected report

This is the finding with the worst consequence, and nothing refuses it at
any point.

An entry's currency is whatever the network reported
(`earnings/lifecycle.go:250-256`: `money.New(report.CommissionMinor,
money.Currency(report.Currency))`). Withdrawal, however, is
deployment-wide single-currency:

- `earnings.Confirmed` selects only entries in the requested currency
  (`reserve.go:107-114`, `ConfirmedEntriesFor` with `Currency`);
- and the request's currency must equal the deployment's payout threshold
  currency, or it is refused (`payout/withdrawal.go:306-309`:
  `ErrCurrencyNotPaid`, *"%s was asked for and payouts are in %s"*).

So a second network reporting in a currency other than
`PAYOUT_THRESHOLD_CURRENCY` produces entries that are credited, confirmed,
counted in the member's wallet — and **can never be withdrawn**. Not
refused, not queued, not flagged. The money is simply unreachable, and every
diagnostic is green.

Note what this is *not*: it is not §4.3, which is one report carrying two
currencies and is refused at the port by `Reported.Validate`. This is one
report, internally consistent, in a currency this deployment cannot pay out.
The port has nothing to say about it, because the port does not know what
this deployment pays in.

The constitution puts **multi-currency wallets out of scope**, which makes
this a *refusal* to design, not a feature: a network whose reports arrive in
a currency the deployment cannot pay must be refused at connection time —
loudly, once, at the point somebody configures it — rather than discovered
by a member who cannot withdraw.

### 4.7 `Limits()` reads a column that reaches no code

The port's own doc comment is explicit (`network.go:349-352`):

> *"The values come from the network's row rather than from literals in the
> adapter, and they are read once, when the composition root builds the
> adapter. So a network that revises its documented limit is a configuration
> change and a restart rather than a release."*

Nothing does this. The fixture returns `defaultLimits()`
(`fixture/fixture.go:195,215`), Awin returns its compiled-in
`DocumentedRateLimitPerMinute = 20` (`awin/client.go:41`), and the one read
of the row is `_, err := q.GetNetwork(ctx, id.String())`
(`connect.go:202`) — an existence check that **discards the row**.

With one network this is a documentation defect. With two it is an operator
trap: `cashback.network` carries `max_query_window_days` and
`rate_limit_per_minute` **per network**, which is exactly the shape two
networks need, and editing either changes nothing. An operator who lowers a
rate limit because a network is complaining will watch us keep hammering it.

Either the values come from the row, or the sentence comes out. They cannot
both stand.

---

## 5. Linkwise

### 5.0 How this section was established, and what it excludes

**Linkwise does not want to be crawled.** `linkwi.se/robots.txt` names
several automated crawlers and disallows each of them, and
`affiliate.linkwi.se/robots.txt` and `go.linkwi.se/robots.txt` are
`Disallow: /` for everyone. The publisher documentation is behind the
affiliate login and stays there. Nothing below was taken from those hosts
against that instruction, and no unauthenticated probe was made against
Linkwise's production API to discover endpoint names.

What is below therefore comes from **third-party integrator source code**
(open-source plugins and workers that talk to Linkwise) and from
**Linkwise's own published JavaScript and its official Chrome extension** —
the extension carries a Chrome Web Store treehash signature block naming
publisher LINKWISE IKE, so it is Linkwise's own code rather than somebody's
reconstruction.

Every claim in §5.2 was **re-fetched from its primary source a second
time**, and read against it rather than for it. Four things did not survive
that, and they are excluded here rather than corrected: three catalogue
fields that actually belong to a different endpoint's response, a set of
example programme ids and amounts presented as quoted when they appear in
no source, a "verified column names" list that is what one integrator
*requested* rather than what the feed *returns*, and a label invented for
the second number in an id pair. Two citations could not be verified at all
and are absent.

**Nothing here is a substitute for FR-105.** A recorded response from a real
publisher account is still what any money field must be mapped from. What
this section changes is the size of the unknown, not its nature.

### 5.1 The network

Linkwise is an affiliate network founded in **2008**, based in Athens,
describing itself as the first and largest in **Greece** and as operating
across SE Europe — roughly 500 advertisers and 15,000 publishers, across
fashion, retail, travel, services, banking and insurance, on its **own
platform since 2012**. It is not a white-label of anything we understand,
and nothing about Awin's shapes transfers to it by assumption.

### 5.2 What a publisher API looks like, from outside

| Surface | What is established | Where from |
|---|---|---|
| **Catalogue** | A REST endpoint `rest_programs.html`, authenticated with **HTTP Basic credentials in the URL**, alongside a separate publisher identifier the integrations call `CD`. The only fields with real evidence are `id`, `logo`, `name`, `short_description` | an open-source WordPress plugin's own source, byte-exact |
| **Deeplinks** | `https://go.linkwi.se/delivery/rest_deeplink.php`, returning results **positionally** — `response[i].deeplink` and `response[i].program` — with a 404/403 dispatch and three distinct "no link" outcomes | Linkwise's own Chrome extension |
| **Per-programme capability** | The `program` object on that response carries `allow_deeplinking` and `access_type` | the same extension |
| **Click URL** | `/z/{program}-{creative}/{CD}/?lnkurl=<destination>` | a real click-URL corpus |
| **SubIDs** | **Exactly five slots**, enforced in Linkwise's own live click script: `subid = function(b,a){b=parseInt(b); if((b>0)&&(b<6)){...}}`. `lnkurl`, `referer` and `rtg` are emitted directly | `go.linkwi.se/delivery/js/crl.js` |
| **Linkwise's own click id** | A 36-character UUID with a **forced version-3 nibble**, matched as `^[0-9A-F]{8}-[0-9A-F]{4}-3[0-9A-F]{3}-[0-9A-F]{4}-[0-9A-F]{12}$`, held in cookie `lkws_<cam_id>` and mirrored across storages | `lwc.js` |
| **Conversion tag** | Current tag builds `aci.html?cam_id=&trans_id=&sale_amount=&adv_subid=&status=` and XHRs to `acx.php`; `load_action` takes **six** arguments; `currency()` and `set_iso_decimal()` append `&currency=` and `&decimal=iso` | live `lwc.js` |
| **Product feeds** | Versioned paths of the form `1.2/<CD>/programs-joined` and `programs-all`, returning an **unpaginated JSON array**; requested columns are **not** the same as returned columns — one integrator verifies exactly one optional field and raises on any other | three independent integrators |
| **Some reporting exists** | A commercial connector offers Linkwise with dimension "Campaign Name" and metrics "Clicks, Commission, Impressions" | that connector's own documentation |
| **Payments** | Notified around the 5th, paid the 15th, requiring the publisher to **accept** in between, and only once validated commissions exceed **EUR 20** | Linkwise's payment FAQ |

### 5.3 What is still unknown, and may not be invented

**No transactions, statistics or payments endpoint was found by either
pass.** Neither the sibling names of `rest_programs.html` nor its
parameters. So the money surface — the only surface `FetchTransactions`
needs — remains exactly as unknown as it was, and this is the list that
matters:

| Unknown | Why it matters |
|---|---|
| The transactions endpoint, its auth, its paging and its cursor semantics | `FetchTransactions` cannot be written |
| Field names and types for sale amount and commission | C-6; `Reported.Validate` |
| Whether sale and commission share a currency | §4.3 — a migration, not an adapter tweak, if they do not |
| **Which currency a report arrives in** | §4.6 — a currency this deployment cannot pay out is a trapped balance |
| The status vocabulary and its mapping | the crediting lifecycle; rule 2 forbids a default |
| Whether a subid slot or the `lkws_` UUID is the join key in reporting | C-2, and the whole attribution path |
| **The maximum length of a subid slot** | `IssuedClickRef` is ≥22 URL-safe characters; a slot shorter than that loses attribution on every click |
| Documented rate limits and query windows | `Limits()` |
| Whether a sandbox exists | whether anything can be recorded before an account is live |

### 5.4 The three things this changes in the plan

**One.** `allow_deeplinking` is a **per-programme flag Linkwise publishes
itself**. That is the evidence for contract rule 11 and for
`merchant_network.can_attribute`: a route the network says may not be
deeplinked cannot carry our reference, so it cannot be the published route.
This replaces an earlier, weaker reading of a third-party summary that
described SubID support as available "when a network/program supports it" —
the conclusion is the same and the evidence is now Linkwise's own.

**Two.** Five subid slots is a **budget**, not a field. Whatever else a
deployment wants to carry through a click — a campaign, a placement, a test
arm — competes with `IssuedClickRef` for one of five, and the reference is
the one that cannot be given up.

**Three.** Linkwise mints its own click identifier and mirrors it across
cookie, `localStorage` and server-side cache. It is a plausible join key in
reporting, and possibly a better one than a subid. Which it is, is a
question a recording answers and nothing else does.

### 5.5 Currency

Greece and Cyprus are euro, and the EUR 20 payment threshold is stated in
euro — so euro is the *likely* reporting currency. It is not established,
Linkwise's stated SE-European reach includes markets that are not euro, and
§4.6 is what makes the difference between "likely" and "established" worth
this sentence: a report in the wrong currency does not fail, it strands a
member's money.

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
| `networks/network.go:198-199` | SC-008's isolation rule "is not written yet" | `internal/arch/network_isolation_test.go` exists (T109) |
| `networks/network.go:349-352` | `Limits()` comes from the network's row | it comes from compiled-in constants; `connect.go:202` discards the row it reads |
