# Port Contracts: Ledger, Network, Payout Rail

**Feature**: [../spec.md](../spec.md) | **Date**: 2026-08-24

Three interfaces carry every external dependency in cashback. All three are
**consumer-defined** — declared by the package that needs them, implemented
in adapter packages, wired in `cmd/`. Each has the same replaceability
budget the constitution sets for the translation adapter: **swappable in
under five engineer-days**, proved by there being a second working
implementation in the repository.

An architecture test fails the build if a vendor type crosses a port
boundary.

---

## 1. `Ledger` — `internal/cashback/wallet`

The only way any code touches money. Nothing outside this package imports a
Blnk type (ADR-0002).

```text
type Ledger interface {
    EnsureAccount(ctx, AccountRef, Currency) (LedgerAccountID, error)
    Post(ctx, Transfer) (TransferRef, error)
    Balance(ctx, LedgerAccountID, Currency) (Amount, error)
    History(ctx, LedgerAccountID, Window) (iterator over Posting, error)
}

type Transfer struct {
    IdempotencyKey string      // required; a Post without one is a programming error
    Postings       []Posting   // must sum to zero per currency
    Reference      string      // entry id / withdrawal request id
    Metadata       map[string]string
}
```

**Contract**

1. `Post` is idempotent on `IdempotencyKey`: replaying returns the original
   `TransferRef` and creates nothing.
2. `Post` rejects a transfer whose postings do not sum to zero per currency,
   before any I/O (C-1 checked twice — in the port and in the ledger).
3. `Post` rejects mixed-currency netting: each currency balances
   independently.
4. Amounts are `int64` minor units with an explicit currency. There is no
   float, no decimal string and no implicit currency anywhere in this
   interface (C-6).
5. `Balance` never returns a cached value the caller could mistake for
   authoritative; the wallet computes member-facing totals from postings.

**Implementations**

| Name | Purpose |
|---|---|
| `blnk` | production (ADR-0002) |
| `memory` | unit tests and `ledger=stub` local development while Docker is unavailable |
| `postgres` | the documented exit route — three tables, a zero-sum trigger, a unique idempotency key |

All three run the **same** conformance suite: idempotent replay, zero-sum
rejection, concurrent post of the same key, balance-after-reversal, and a
crash injected between Apivo's commit and the ledger call (spike S2).

---

## 2. `Network` — `internal/cashback/networks`

One package per network; nothing outside an adapter knows a network's
vocabulary (ADR-0003).

```text
type Network interface {
    ID() NetworkID
    BuildDeeplink(ctx, Offer, ClickRef) (string, error)
    FetchTransactions(ctx, Window) (iterator over Reported, error)
    FetchCatalogue(ctx) (iterator over ReportedMerchant, error)
    Limits() Limits   // max query window, requests per second
}

type Reported struct {
    ExternalID    string
    ClickRef      string        // empty when the network reported none
    StatusRaw     string        // verbatim
    Status        Status        // pending | confirmed | declined | reversed
    SaleAmount    Amount
    Commission    Amount
    TransactedAt  time.Time
    RawPayload    json.RawMessage   // required, never empty
}
```

**Contract**

1. `RawPayload` is the verbatim network response fragment for that
   transaction. An adapter returning an empty payload fails its conformance
   test (FR-032).
2. `Status` mapping is total: an unrecognised `StatusRaw` is an error, never
   a silent default. Unknown statuses must surface to an operator.
3. `FetchTransactions` respects `Limits()`: it never issues a window wider
   than the network allows (31 days for Awin) and never exceeds the declared
   request rate.
4. Iteration is resumable: a caller that stops mid-iteration and restarts
   from the same `Window` sees the same set. Cursors advance only after a
   window is fully persisted (FR-031).
5. `BuildDeeplink` places `ClickRef` in the network's own click-reference
   parameter (`clickref` for Awin) and returns an error rather than a
   partially-formed URL.
6. Adapters never write to the database and never decide credits. They
   translate, and nothing else.

**Implementations**: `fixture` (recorded lifecycle: click → pending →
approved → reversed) plus one real adapter once the founder answers Q1.
The `fixture` adapter is what un-blocks the build from the network decision.

**Conformance suite** (runs against every adapter): status mapping totality,
raw payload presence, window clamping, rate-limit adherence under
concurrency, deeplink round-trip, resumable iteration, and a live contract
test that is **skipped** unless credentials are present — the same posture
the repository already takes with `DATABASE_URL`-keyed tests.

---

## 3. `PayoutRail` — `internal/cashback/payout`

```text
type PayoutRail interface {
    Kind() RailKind
    Submit(ctx, PayoutInstruction) (RailReference, error)
    Status(ctx, RailReference) (RailStatus, error)
}

type PayoutInstruction struct {
    IdempotencyKey string    // derived from the withdrawal request id (D8)
    Amount         Amount
    Destination    DestinationRef
    Descriptor     string    // rendered from brand config (FR-073)
}
```

**Contract**

1. `Submit` is idempotent on `IdempotencyKey`. A rail that cannot guarantee
   this must implement it locally before returning.
2. Failures are classified: `Retryable` reuses the same key;
   `Terminal` releases the reservation back to confirmed balance and tells
   the member (FR-053, US4 scenario 5).
3. A rail never sees a member's identity beyond the destination reference it
   needs.
4. `Descriptor` comes from brand configuration — no rail implementation
   contains a product name (FR-070).

**Implementations**: `manual` (an operator executes the transfer and records
the reference; still enforces C-4 and C-5) and `stub` (tests, including
timeout, duplicate submission and permanent failure). A real rail follows
the founder's answer to Q5.

---

## 4. Shared rule

For all three ports: **the domain owns the interface, the adapter owns the
vendor.** A change of vendor is a new package plus a wiring line in `cmd/`.
If a swap would require touching domain code, the port is wrong and the port
gets fixed — not the domain.
