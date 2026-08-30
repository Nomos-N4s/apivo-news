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
    Account() PublisherAccount            // which publisher account this adapter polls
    BuildDeeplink(ctx, DeeplinkTarget, IssuedClickRef) (string, error)
    FetchTransactions(ctx, QueryWindow) (iterator over Reported, error)
    FetchCatalogue(ctx) (iterator over ReportedMerchant, error)
    Limits() Limits   // max query window, requests per second
}

type Reported struct {
    ExternalID    string
    ClickRef      ClickRef      // what the network echoed, or the definite absence of one
    StatusRaw     string        // verbatim
    Status        Status        // pending | confirmed | declined | reversed
    SaleAmount    Amount
    Commission    Amount        // same currency as SaleAmount — one column stores both
    TransactedAt  time.Time
    RawPayload    json.RawMessage   // required, never empty
}
```

`ClickRef` and `IssuedClickRef` are deliberately two types. A `ClickRef` is
what a network echoed back — arbitrary text, legitimately absent, refused
only when it is present and blank — and it encodes as a JSON string or
`null`, never as an empty string standing in for absence. An
`IssuedClickRef` is the reference **Apivo** minted before the redirect, and
it cannot be constructed unless it satisfies
`click_ref_url_safe_and_long_enough` (FR-020). One type for both would let
reconciliation code re-issue a redirect with a reference no `click` row can
match.

`DeeplinkTarget` is the four facts a redirect is assembled from — offer id,
network id, click-reference parameter, template — and not the catalogue's
`Offer`. The `Offer` is mapped out of a generated sqlc store, so a port
taking one puts the Postgres driver in every adapter's dependency graph, and
it carries the commercial band, which rule 6 says an adapter decides nothing
from.

`QueryWindow` is not called `Window`: `wallet.Window` exists in the same
domain with the opposite zero value (there a zero bound is "unbounded", here
either is refused), and the poller touches both.

**Contract**

1. `RawPayload` is the verbatim network response fragment for that
   transaction. An adapter returning an empty payload fails its conformance
   test (FR-032). A JSON `null`, an empty object or array, and a bare scalar
   are all refused as absence in costume, and so are bytes the `jsonb`
   column will not take — not UTF-8, or a `\u0000` or lone-surrogate escape,
   both of which `json.Valid` accepts.
2. `Status` mapping is total: an unrecognised `StatusRaw` is an error, never
   a silent default. Unknown statuses must surface to an operator.
3. `FetchTransactions` respects `Limits()`: it never issues a window wider
   than the network allows (31 days for Awin) and never exceeds the declared
   request rate. The width half is checked by the port
   (`Limits.ValidateWindow`); the rate half belongs to the adapter's limiter
   (T056) and is asserted by the conformance suite, not by a type.
4. Iteration is resumable: a caller that stops mid-iteration and restarts
   from the same `QueryWindow` asks the same question and misses nothing.
   Cursors advance only after a window is fully persisted (FR-031). This is
   **not** a promise of an identical answer: a re-issued window returns the
   network's account of that period as it stands now, and it must, because
   the trailing re-read is the only mechanism by which a pending transaction
   is ever seen to become confirmed (ADR-0003). An adapter that memoised a
   window's pages would satisfy resumability to the letter and freeze every
   member's money at pending.
5. `BuildDeeplink` places the issued click reference in the network's own
   click-reference parameter (`clickref` for Awin) and returns an absolute
   http/https URL or an error, never a partially-formed URL. Every refusal
   wraps `ErrDeeplinkNotFormed`; a refusal that is deterministic — our own
   routing bug, or a route somebody has to fix — also wraps
   `ErrDeeplinkInputsRefused`, so the click-out handler's 502 does not page
   the on-call towards a network that is working perfectly.
6. Adapters never write to the database and never decide credits. They
   translate, and nothing else. Nothing in the port can hold an adapter to
   this; what the port does is withhold the means — no signature speaks a
   database type and the port file imports no driver and no generated store,
   which a test in `internal/cashback/networks` refuses. The
   repository-wide rule that seals an adapter inside its own package
   (SC-008) is T109's and is not written yet.
7. Every value an adapter yields has passed its own `Validate` before it is
   yielded. Among those rules: a report's sale and commission carry the
   **same** currency, because the evidence row stores one `currency` column
   for both figures, so a report denominating them differently cannot be
   stored without one of the two being silently restated.
8. Iteration that ends early says so. An adapter that stops for its own
   reason — a cancelled context, a spent retry budget — yields one final
   pair carrying `ErrIterationAbandoned` and returns; only a caller's own
   break ends a sequence silently. A range loop that ends having yielded no
   error therefore means the answer was whole, which is what rule 4 lets a
   cursor advance on, and what an import may reconcile an absent retailer to
   `left_network` on.
9. Failures against the network are classified, as `PayoutRail`'s are:
   `ErrNetworkUnavailable` is retryable, `ErrNetworkRateLimited` is
   retryable after waiting, `ErrNetworkRefused` is terminal until somebody
   changes a credential. Rule 4 offers no resumption point inside a window,
   so the only response to a mid-window failure is to re-run the whole
   window — correct for the first two, an infinite loop with a frozen cursor
   for the third. The immediate error of either iterator carries only what
   is checkable **without** contacting the network; everything from
   contacting it is yielded, so an eager adapter and a lazy one report an
   expired credential through the same channel.
10. One adapter serves one publisher account. `network_account` is unique on
   `(network_id, external_publisher_id)` and each row carries its own
   cursors, so a deployment with two Awin accounts wires two adapters and
   the poller keys them by `Account().ID()` — never by `ID()`, which both
   share. `Account().ID()` is also the `network_account_id` every evidence
   row requires. `ValidateNetwork` holds an adapter to all of this at
   wiring: a valid id, a real account, that account at this adapter's own
   network, and usable limits.

**Implementations**: `fixture` (recorded lifecycle: click → pending →
approved → reversed) plus one real adapter once the founder answers Q1.
The `fixture` adapter is what un-blocks the build from the network decision.

**Conformance suite** (runs against every adapter): status mapping totality,
raw payload presence, window clamping, rate-limit adherence under
concurrency, deeplink round-trip, resumable iteration, that every yielded
value has passed its own `Validate` (rule 7), that a cancelled context ends
iteration with `ErrIterationAbandoned` rather than quietly (rule 8), and a
live contract test that is **skipped** unless credentials are present — the same posture
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
