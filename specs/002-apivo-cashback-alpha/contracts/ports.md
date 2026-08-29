# Port Contracts: Ledger, Network, Payout Rail

**Feature**: [../spec.md](../spec.md) | **Date**: 2026-08-24

Three interfaces carry every external dependency in cashback. All three are
**consumer-defined** — declared by the package that needs them, implemented
in adapter packages, wired in `cmd/`. Each has the same replaceability
budget the constitution sets for the translation adapter: **swappable in
under five engineer-days**, proved by there being a second working
implementation in the repository.

An architecture test fails the build when a vendor type crosses a port
boundary. What is enforced today is the ledger half of that rule
(`internal/arch`): only a package directly beneath
`internal/cashback/wallet` may import a ledger vendor's SDK — the port
package itself included in the refusal. The network and payout ports get
the same rule with the packages they describe, and until they exist the
claim above is a statement of intent for them and an enforced rule for the
ledger.

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
   `TransferRef` and creates nothing. **The same transfer** means the same
   *multiset* of `(account, amount)` movements — posting order is
   representation, not identity — plus an identical `Reference` and
   identical `Metadata`, with a nil map and an empty map both meaning "no
   annotations". Anything else under a used key is
   `ErrIdempotencyConflict`.
2. `Post` rejects a transfer whose postings do not sum to zero per currency,
   before any I/O (C-1 checked twice — in the port and in the ledger).
3. `Post` rejects mixed-currency netting: each currency balances
   independently. It also rejects a posting whose currency is not its
   account's, wrapping `money.ErrCurrencyMismatch` — an account is one
   currency (C-6), and no implementation converts, picks a rate, or
   re-denominates.
4. Amounts are `int64` minor units with an explicit currency. There is no
   float, no decimal string and no implicit currency anywhere in this
   interface (C-6).
5. `Balance` is the ledger's own authoritative figure, consistent with the
   postings it recorded and never a number Apivo stored beside them; the
   wallet computes member-facing totals from postings and the two are
   cross-checked to the minor unit (D7). Whether the ledger sums on demand
   or maintains the figure as it posts is the ledger's business — what is
   forbidden is a second truth outside it.
6. `Post` rejects a transfer that balances by moving nothing — every
   account it touches netting to zero — wrapping `ErrNoMovement`. That is
   the shape a caller bug makes when source and destination resolve to one
   account, and a `TransferRef` for it is proof of a payment that never
   happened.
7. `Post` never leaves a **member stage account** negative: refused
   wrapping `ErrInsufficientFunds`, judged against the balance at the
   moment the transfer is applied. This is the mechanism behind D9 —
   nothing else refuses the second concurrent reservation. **House
   accounts are exempt**: they are the boundary of the closed set of
   accounts, and a ledger where nothing may go negative cannot fund its
   first credit.
8. A transfer this port admits but an implementation cannot record in one
   atomic act is refused wrapping `ErrUnsupportedTransfer` — never posted
   in instalments. The production substrate denominates a transaction in a
   single currency, so a cross-currency transfer is that refusal there;
   callers split it into one transfer per currency, each with its own key.
   Within one currency the shape is unrestricted: any number of accounts
   giving, any number receiving.
9. What `Post` recorded is readable the moment it returns: a `Balance` or
   `History` call beginning afterwards sees the transfer's postings. An
   implementation whose substrate records asynchronously does not return
   until it has stopped being asynchronous.
10. `History` orders by ascending `PostedAt` with ties broken by recording
    order, stably across repeated calls. Ties are ordinary — every posting
    of a transfer shares its instant, and a substrate may store instants
    coarsely (Postgres truncates to microseconds) — so watermark
    resumption is **at-least-once**: a reader resumes from the `PostedAt`
    of the last posting it consumed, inclusive, and is idempotent.

**Implementations**

| Name | Purpose |
|---|---|
| `blnk` | production (ADR-0002) |
| `memory` | unit tests and `ledger=stub` local development while Docker is unavailable |
| `postgres` | the documented exit route — three tables, a zero-sum trigger, a unique idempotency key |

All three run the **same** conformance suite: idempotent replay (including
a replay whose postings are rebuilt in a different order, which must be a
replay, and one whose metadata differs, which must be a conflict), zero-sum
rejection, wrong-currency posting rejection, no-movement rejection,
concurrent post of the same key, two concurrent reservations against one
confirmed balance where exactly one wins, read-your-writes after `Post`,
balance-after-reversal, and a crash injected between Apivo's commit and the
ledger call (spike S2). An implementation that refuses a shape with
`ErrUnsupportedTransfer` declares which shapes those are, and the suite
holds it to refusing them rather than mis-posting them.

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
