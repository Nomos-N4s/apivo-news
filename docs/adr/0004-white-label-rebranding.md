# ADR-0004: White-label architecture — one brand configuration, zero brand literals

- **Status**: Accepted (2026-08-24)
- **Date**: 2026-08-24
- **Deciders**: founder
- **Related**: [0001](0001-super-app-architecture.md), [0005](0005-cashback-stack.md)

## Context

The founder requires the cashback system to be **ready to be rebranded**.
Two readings are possible and they cost very different amounts:

1. **Rebrandable** — the same deployment can be renamed, restyled and
   re-domained without touching code. One brand live at a time.
2. **Multi-tenant white label** — many brands live simultaneously in one
   deployment, each with its own members, merchants, balances and legal
   entity.

Reading 2 is a different product: it puts a tenant column on every table,
tenant scoping on every query, tenant isolation on every test, and a tenant
dimension in every operator screen. Done later it is expensive; done now,
before there is a single paying member, it is expensive **and** speculative.

The repository already carries the hard half of the problem in a good state:
language is a first-class axis (constitution VII) and place is separate from
it, so localisation is not entangled with branding.

## Decision

**Build for reading 1 — single-brand-per-deployment, fully configuration
driven — and make reading 2 a schema migration rather than a rewrite.**

### One brand configuration, one source

A single brand definition is loaded once at start-up on both sides:

- Go: `internal/platform/brand` exposes a `Brand` value through the
  composition root; no package reads brand data from anywhere else.
- Astro: `web/src/lib/brand` loads the same file at build time and exposes
  typed tokens plus CSS custom properties.

It carries: product name and legal entity, domains and support addresses,
logo and favicon asset paths, colour and typography tokens, default
language and place, default currency, payout descriptor text, legal document
identifiers and terms version, and per-product feature flags.

### Zero brand literals — enforced, not requested

- **No product name, colour, domain, email address or currency literal in
  application code, templates or migrations.** A CI lint greps the source
  tree for the current brand's proper nouns and fails on a hit outside the
  brand configuration and its assets.
- All member-facing text lives in translation catalogues keyed by BCP-47
  primary language subtag (FR-071); the brand name enters copy only as an
  interpolated token, never as part of a translated string.
- Emails, PDFs, legal pages and payout descriptors render from the same
  brand values (FR-073).

### Proof by fixture brand in CI

A fixture brand (`testdata/brand/fixture/`) with a deliberately different
name, palette, currency and default language renders every member-facing
surface in CI (FR-072, SC-007). Rebrandability is therefore a test that goes
red, not a claim in a document.

### Forward compatibility with multi-tenancy

Two cheap structural choices keep reading 2 reachable:

1. **Brand id is carried in the domain from day one** as a value on the
   records where a tenant boundary would eventually fall — member
   participation, merchant availability, cashback entry, payout — populated
   with the single active brand. Adding tenants later means changing what
   fills the column and adding scoping, not discovering where the column
   should go.
2. **No global mutable singleton for the brand.** It is passed explicitly
   from the composition root, so a per-request brand resolver is a change at
   one seam.

Multi-tenant isolation, per-tenant secrets, per-tenant ledgers and
per-tenant operator scoping are **explicitly not built**.

## Consequences

**Positive**

- A rebrand is a configuration change plus an asset swap, provable in CI.
- The founder can trial a name without a code fork.
- Localisation and branding stay independent, so a brand change does not
  disturb the language axis.
- The multi-tenant door stays open at the cost of one column and one
  discipline.

**Negative / accepted costs**

- The lint is noisy the first time it runs and needs an allowlist for
  genuine historical references (licences, changelogs).
- Carrying a brand id that is always the same value looks like dead weight
  until the day it is not. That day may never come; the cost is one small
  column.
- Contributors must add strings to catalogues rather than templates, which
  is friction on every copy change.

## Alternatives considered

| Alternative | Why rejected |
|---|---|
| **Full multi-tenancy now** | Speculative complexity in every query, test and screen before a single member exists. Contradicts the constitution's "complexity must justify itself against the alpha scope". |
| **Theming only (CSS variables), name stays in code** | Fails the actual requirement: a rebrand includes the name, domain, legal entity and payout descriptor, which is where the real work hides. |
| **Runtime brand switching by hostname** | Multi-tenancy in disguise, with none of its safeguards — one shared member table and one shared ledger behind two brands is a data-protection incident waiting to be written up. |
| **Build-time templating (find and replace at release)** | Produces per-brand build artefacts and per-brand bugs, and cannot be tested in one CI run. |
| **A brand microservice** | A configuration file does not need a network hop. |

## Revisit triggers

- The founder needs two brands live simultaneously.
- A partner asks for a white-label deployment with its own legal entity.
- The brand configuration starts holding behaviour rather than values —
  the first conditional keyed on brand id is the signal that tenancy is
  arriving.
