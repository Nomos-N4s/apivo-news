# ADR-0005: Cashback stack — no second stack; Go and Astro, as today

- **Status**: Accepted (2026-08-24)
- **Date**: 2026-08-24
- **Deciders**: founder
- **Related**: [0001](0001-super-app-architecture.md), [0002](0002-cashback-money-substrate.md)

## Context

The founder is explicitly open to a different stack for cashback than the
one epiloYES runs (Go modular monolith, Astro SSR, Supabase Postgres,
sqlc-generated types both sides). The question deserves a real answer rather
than an appeal to consistency, because cashback genuinely has different
characteristics from news: money arithmetic, third-party API adapters,
scheduled reconciliation, an operator back office.

The candidate stacks worth weighing were Go (as today), TypeScript/Node
end-to-end, Elixir/Phoenix, and Java or Kotlin with Spring.

The decisive constraint is not technical fit. It is that **one person builds
and operates all of it**, and every stack has a fixed tax — CI, linting,
dependency updates, security patching, build images, editor tooling,
deployment, debugging know-how — that is paid per stack, forever,
regardless of how much code is in it.

## Decision

**Cashback is built in Go and Astro, in the existing repository, with the
existing type-generation pipeline. No second stack.**

Fit, not just consistency:

- **Money in Go.** Integer minor units in `int64`, exhaustive error
  handling, no implicit numeric coercion. The language's weaknesses
  (verbosity, thin generics) are irrelevant to a domain that is arithmetic
  and state machines; its strengths are exactly on point.
- **Adapters in Go.** Network adapters are HTTP clients with rate limiting,
  retry, backoff, cursor management and long-running pollers. Goroutines,
  `context` cancellation and `errgroup` make this ordinary rather than
  clever.
- **Ledger in Go.** Blnk (ADR-0002) is Go with a Go SDK, so the substrate
  and its consumer share a language, a build and a test harness.
- **Schema-generated types on both sides.** The constitution's rule — types
  are generated, never hand-written on both sides, with CI failing on drift
  — already has a working implementation (sqlc, `supabase gen types`).
  Cashback inherits it for free; a second stack would need its own.
- **Astro for the member surface.** Wallet and catalogue are
  content-shaped, SEO-irrelevant behind auth, and benefit from SSR. The
  wallet's interactive parts are islands, not an SPA.

Additions this decision does accept:

- **Redis**, required by Blnk (ADR-0002). It holds no source of truth.
- **A decimal-free money type** in `platform/money` — minor units plus ISO
  4217 currency, with explicit rounding modes. No third-party money library
  and no floating point anywhere in the codebase.
- **A scheduler** for pollers and reconciliation runs, in-process, with a
  Postgres advisory lock so two instances cannot double-run a job.

### Operator back office

The back office is Astro pages behind the existing role check, not a second
application and not a third-party admin framework. Queues are lists with
actions; every action records a named human and a reason (FR-061). This is
deliberately unglamorous: at alpha volume a good list beats a framework.

## Consequences

**Positive**

- One toolchain, one CI matrix, one set of coverage gates, one deployment
  story, one debugging skillset.
- Cashback inherits the platform kernel, identity, brand, logging, config
  and migration machinery on day one instead of reimplementing it.
- Shared code that must not diverge — money formatting, place resolution,
  language fallback — physically cannot diverge, because there is one
  implementation.

**Negative / accepted costs**

- Go's back-office ergonomics are worse than a batteries-included framework
  (Rails, Phoenix, Django admin) would offer. Real cost, paid in hand-built
  operator screens, accepted because the alternative is a second stack tax.
- No opportunity to evaluate a new stack on a real workload. Accepted: the
  MVP is the wrong place to buy that option.
- The 90% Go coverage gate applies to cashback too, including adapter code
  that is mostly plumbing. Fixtures make it achievable; it is still work.

## Alternatives considered

| Alternative | Why rejected |
|---|---|
| **TypeScript/Node end-to-end for cashback** | Genuinely attractive for sharing types with Astro and for back-office ergonomics. Rejected because money arithmetic in JavaScript needs a decimal library and constant vigilance about `number`, and because it would split backend knowledge across two runtimes for one person. |
| **Elixir/Phoenix** | Excellent for concurrent pollers, LiveView is a strong back-office answer, and the fault-tolerance story is real. Rejected purely on the single-operator tax: a second BEAM toolchain, deployment model and debugging skillset for one product. |
| **Java/Kotlin + Spring** | The richest ecosystem for financial domains. Rejected on operational weight and on the founder having no other JVM workload. |
| **Adopt a full commerce platform (Medusa, Vendure, Saleor) and bend its promotions engine into cashback** | These model selling goods. Cashback models a claim against a third party's future payment. The impedance mismatch would be permanent, and it drags a large runtime along for a small subset of features. |
| **Separate Astro app for cashback** | Splits session handling, the design system and the robots/indexing gate across two deployables to solve a problem — route organisation — that a directory solves. |
| **Third-party admin framework for the back office** | Every one assumes direct table access, which is exactly what C-1 to C-7 forbid; operator actions must go through the domain so their audit records exist. |

## Revisit triggers

- A second engineer joins with deep expertise in another stack **and** owns
  a product end to end.
- The back office grows past what hand-built pages sustain.
- A cashback workload appears that Go genuinely serves badly (none is
  foreseen at this scope).
