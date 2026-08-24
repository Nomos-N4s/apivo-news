# ADR-0003: Affiliate network integration — consumer-defined adapters, fixtures first

- **Status**: Accepted (2026-08-24)
- **Date**: 2026-08-24
- **Deciders**: founder
- **Related**: [0002](0002-cashback-money-substrate.md)

## Context

Commission is the only revenue in this product, and it arrives entirely from
third-party affiliate networks. Networks differ in every dimension that
matters: authentication, pagination, rate limits, query-window limits,
status vocabulary, currency handling, deeplink construction and the name of
the click-reference parameter.

Awin, taken as the reference because it is the network most used by European
cashback publishers, sets concrete constraints the design must absorb:

- Publisher transaction API, with per-transaction pull and status checks.
- **Rate limit of 6 requests per second.**
- **Maximum 31 days between `startDate` and `endDate`** on a transaction
  query — so backfill is inherently windowed.
- Publisher-to-advertiser tracking via a **`clickref`** parameter (plus
  `clickref2..6`), which is how a member's click is recognised on the way
  back.
- Status vocabulary **pending → approved | declined**, where pending means
  awaiting advertiser validation, and validation may take up to 90 days.
- Real-time transaction notifications exist and are, in Awin's own framing,
  popular with loyalty and cashback publishers.

None of this is stable across networks, and the founder has not yet chosen
which networks to join (spec question Q1) — publisher accounts require
application and approval, which is a business process with a lead time.

## Decision

**One consumer-defined interface, one package per network, no live
credentials required to build or test.**

### The port

`internal/cashback/networks` defines the interface the domain needs, not the
interface any network offers:

```text
type Network interface {
    ID() NetworkID
    BuildDeeplink(ctx, offer, clickRef) (url, error)
    FetchTransactions(ctx, window) (iterator over NormalisedTransaction, error)
    FetchCatalogue(ctx) (iterator over NormalisedMerchant, error)
}
```

Adapters live in `internal/cashback/networks/<name>/` (`awin/`,
`tradedoubler/`, …). Nothing outside an adapter package knows a network's
vocabulary. An architecture test fails the build if a network-specific type
escapes its package — which is how SC-008 ("adding a second network changes
only its adapter") is proved rather than asserted.

### Normalisation is one-way and lossless

Every adapter emits a `NormalisedTransaction` **and** the verbatim raw
payload. The raw payload is stored with the record (C-3, immutable). When
normalisation is later found to be wrong, the fix re-derives from stored raw
payloads; nothing has to be re-fetched from the network, which may no longer
serve it.

Network status maps into one domain state machine —
`pending → confirmed | declined`, with `reversed` reachable from either.
The mapping table lives in the adapter and is unit-tested against recorded
fixtures.

### Polling: cursor per network account, windowed, rate-limited

- A durable cursor per network account, advanced only after a window is
  fully persisted, so a restart re-fetches at most one window and never
  skips one (FR-031).
- Windows are capped at each network's documented maximum (31 days for
  Awin), and backfill walks windows backwards from the cursor.
- Request rate is governed by a per-adapter limiter configured from the
  network's documented limit, with exponential backoff and jitter on 429 and
  5xx.
- Because validation can take up to 90 days, the poller **re-reads a
  trailing window** (default 100 days) on a slower schedule to catch status
  changes, not only new transactions.

### Push notifications are additive, never authoritative

Where a network offers real-time notifications, they are treated as a
**hint that shortens latency** — they trigger an immediate targeted poll.
A credit is only ever created from a polled, stored, verified record. A
webhook payload alone never moves money; this removes webhook authenticity
from the trust chain entirely.

### Fixtures first: build with no credentials

Every adapter ships with recorded response fixtures under
`testdata/`, and the whole ingestion chain is testable offline. A
`network=fixture` mode serves a scripted lifecycle (click → pending →
approved → reversed) so the wallet, entry state machine, reconciliation and
payout paths can be built and demonstrated before any publisher account is
approved.

This is what un-blocks the plan from founder question Q1: **implementation
does not wait for the network choice; only go-live does.**

### Credentials

Network credentials never enter the database or the repository. They are
read from the environment through `platform/config`, one set per network
account, and are redacted in logs by the existing logging package.

## Consequences

**Positive**

- The founder's network choice (Q1) stops being a blocker for engineering.
- Adding a network is a bounded, reviewable unit of work with a
  build-enforced blast radius.
- Storing raw payloads means normalisation bugs are recoverable without the
  network's cooperation.
- Refusing to trust webhooks removes a whole class of forgery and replay
  risk from the money path, at the cost of some latency.

**Negative / accepted costs**

- Fixtures drift from live APIs. Mitigated by a contract test per adapter
  that runs against the live API only when credentials are present, and is
  skipped otherwise — the same posture the repository already takes with
  `DATABASE_URL`-keyed tests.
- Trailing re-reads cost API budget. At alpha volume this is negligible;
  the window is configuration.
- Latency is a poll cycle rather than instant, even where notifications
  exist. Acceptable: SC-001 asks for a wallet entry within 24 hours.

## Alternatives considered

| Alternative | Why rejected |
|---|---|
| **Webhook/postback-driven credits** | Makes payload authenticity part of the money path, and networks' postbacks are not uniformly signed. Kept as a latency hint only. |
| **A third-party aggregator API (Strackr, wecantrack)** | Adds a paid dependency between Apivo and its only revenue source, and does not remove the need for a normalised domain model. Excluded by the free/open-source directive. |
| **One generic configurable adapter driven by a mapping file** | Networks differ in behaviour (pagination, retry semantics, deeplink signing), not just field names. A configuration language expressive enough to cover them becomes a worse programming language. |
| **Build the first adapter directly against the domain, extract later** | The extraction never happens under delivery pressure, and the first network's vocabulary silently becomes the domain's. The interface costs a day now. |
| **Wait for the founder's network decision before building** | Stalls all cashback engineering behind a business process with an unknown lead time, for no engineering benefit. |

## Revisit triggers

- The first live network is approved and its real behaviour contradicts a
  fixture-derived assumption.
- A network offers signed, replay-protected notifications that would be safe
  to treat as authoritative.
- More than three adapters exist and duplication between them becomes
  visible.
