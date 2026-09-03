# Port Contract Delta: What a Second Real Adapter Is Held To

**Feature**: [../spec.md](../spec.md) | **Base**: `002/contracts/ports.md`

The `Network` port does not change shape. It is already
consumer-defined, already six methods, and already keyed by
`PublisherAccount` rather than by network — its own doc comment says a
deployment running two Awin accounts wires two adapters and the poller keys
them by `PublisherAccount.ID`, *"never by `Network.ID`, which both of them
share"* (`networks/network.go:161-167`).

So this file records four things: one rule that was claimed and not
asserted, one rule that is new, one clarification, and the change that makes
the suite mean something.

---

## 1. Rule 7 was asserted by nothing — repaired before this feature begins

Rule 7 says every value an adapter yields has passed its own `Validate`, and
both the port's doc comment and `002/contracts/ports.md:244` say **the
conformance suite asserts it**. It did not: the only `Validate` call in the
suite was on `Limits`.

Demonstrated rather than asserted: an adapter that skips `Validate` and
yields a report whose sale and commission carry **different currencies**
passed **eleven of the twelve** scenarios. The twelfth failed only because
the fixture itself calls `Validate` and refuses to yield.

Filed as [#496](https://github.com/Nomos-N4s/apivo-news/issues/496), closed
by [#497](https://github.com/Nomos-N4s/apivo-news/pull/497), **before** any
second adapter exists. The order matters: the suite is the thing a second
adapter is held to, and a suite with a hole in it certifies the hole.

---

## 2. New — Rule 10: a route says whether it can be attributed

### Why this is a contract rule and not an adapter detail

Linkwise's own deeplink response carries **`allow_deeplinking`** on each
programme — a per-programme flag the network publishes about itself
([../research.md](../research.md) §5.2). Whether a route can carry our click
reference is therefore the network's answer, per programme, and not
something we may assume network-wide.

A route that cannot carry a click reference is not a broken route. It works
perfectly: members click, members buy, the network pays the publisher. What
it cannot do is tell us **whose** purchase it was. Every transaction from it
arrives unattributed *by construction*.

Publishing a cashback rate on such a route is the worst failure this product
has: a member clicks a promised percentage, buys, and is never credited —
and every diagnostic looks healthy, because nothing failed.

### The rule

> **10. A catalogue entry states whether its route can carry a click
> reference.** An adapter that knows a programme cannot carry one yields the
> route with that fact set, rather than omitting the route or yielding it as
> if it could. An adapter with no way to know says so once, for the whole
> network, rather than guessing per route.

Three things follow, and each is deliberate:

- **The route is yielded, not dropped.** An operator needs to see that a
  retailer is reachable-but-unattributable; a dropped route is
  indistinguishable from a retailer the network does not carry.
- **The catalogue never publishes it** — enforced in the database, not by
  the importer (see [../data-model.md](../data-model.md) 0034).
- **"I don't know" is not "yes".** An adapter that cannot determine
  attributability per route declares it network-wide. Defaulting to
  attributable is what produces the silent failure above.

This composes with rule 5 rather than replacing it. `BuildDeeplink` still
returns an error wrapping `ErrDeeplinkNotFormed` rather than a URL with no
reference in it — rule 10 is what stops a member ever reaching that
deeplink, and rule 5 is what stops the mistake being survivable if they do.

---

## 3. Clarification of Rule 3: every adapter's limiter is its own

Rule 3 already says an adapter *"paces every request it makes to the
declared rate, which is the half its limiter holds"*. With one adapter that
sentence has only one reading. With two it acquires a wrong one, so it is
made explicit (FR-106):

> Each adapter holds **its own** rate limiter and **its own** retry budget.
> Neither is shared between networks, and neither is derived from a global.
> One network rate-limiting us must not slow another, and one network
> refusing must not spend another's retries.

No signature changes. This is a statement about construction, and the
conformance suite asserts it the only way a suite can: by running two
adapters concurrently and checking that a `429` from one does not delay the
other.

---

## 4. The suite runs over what the binary ships

Today the conformance suite runs against the fixture. That is how it was
written and it was correct when the fixture was the only implementation.

**Change**: the suite becomes a table over the **shipped driver registry**
(`plan.md` D-B), so an adapter compiled into the binary is an adapter the
suite runs. An adapter that is not run by the suite becomes a compile-time
omission — someone has to have deliberately left it out of the registry, and
then it does not ship either.

```text
for each driver in registry:
    for each of the 12+ conformance scenarios:
        run it against that driver's adapter, over its recorded testdata
```

**Why the registry and not a hand-kept list in the test**: a hand-kept list
is a second place to remember, and the whole defect this feature removes is
two places that disagreed about which drivers exist
(`cmd/apivo/networks.go:138` vs `cmd/apivo/connect_network.go:190`).

**Fixtures, not live calls.** The suite runs against each adapter's recorded
`testdata/`, per FR-105. `002/contracts/ports.md` §2 describes *"a live
contract test, skipped without credentials"* — no such test exists
([../research.md](../research.md) §6), and this feature does not add one: a
test that is skipped in CI and green when skipped is a test that reports the
absence of credentials as success.

---

## 5. What does **not** change

| Thing | Why it stays |
|---|---|
| The six methods | Nothing a second network needs is missing from them |
| `Account()` as identity, `ID()` as vocabulary | Already correct; two accounts on one network already work |
| Rules 1, 2, 4, 5, 6, 8, 9 | Each is network-agnostic and already asserted |
| `Reported.Validate` requiring one currency | This is C-6, and Q13 is where a network that violates it gets decided — not here |
| The port's freedom from database types | Rule 6, and now sealed structurally by `internal/arch/network_isolation_test.go`, which exists (the doc comment saying it "is not written yet" is stale — see [../research.md](../research.md) §6) |
