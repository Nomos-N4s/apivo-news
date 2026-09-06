# Implementation Plan: Cashback Claims

**Spec**: [spec.md](spec.md) · **Constitution**: v1.2.0, amendment pending
(see spec §Governance Impact)

**Status**: Draft — no task in Phase 2 or later starts until the amendment is
ratified. Phase 1 is drafting work on this feature's own documents and is safe
to do now.

## Technical Context

The claim surface is the first thing in cashback that takes a member's own
words and a member's own file. Everything before it was machine evidence: a
click we recorded, a transaction the network sent. That difference decides most
of the design below.

- **Language/runtime**: Go 1.25 backend, Astro 7 + TypeScript strict frontend,
  as elsewhere.
- **New domain package**: `internal/cashback/claims/`. It sits inside the
  cashback product domain, so it may read that domain's own tables and use the
  platform kernel; it may not import another product domain at any depth.
- **New storage dependency**: object storage for evidence files. This is the
  first one in the repository, and it goes behind a consumer-defined port with
  a second working implementation, per the Architecture Constraints.
- **Database**: two new migrations in the `cashback` schema. No foreign key
  crosses a product schema boundary; the member is referenced by the same
  account id every other cashback table uses.

## The shape of the decision

The classification in FR-105 is the load-bearing part, so it is computed in one
place and returned by one read, rather than being assembled by the operator
screen out of three separate lookups. The screen renders a verdict object; it
does not derive one. Two reasons:

1. An operator screen that derives the case can derive it differently from the
   handler that enforces which remedies are legal, and then the button that is
   shown and the action that is permitted disagree. Computing once and
   enforcing against the same value removes the disagreement rather than
   testing for it.
2. The classification is the thing SC-102 has to be able to replay. A value
   that was computed and stored at decision time answers "what did we know
   when we decided"; a value the screen recomputed answers "what do we know
   now", which is not the question an audit asks.

So: `claim_decision` stores the classification and the three record rows as
they read **at decision time**, alongside the outcome. The live screen shows
the current computation; the record keeps the historical one.

## Ports

| Port | Why it is a port | Second implementation |
|---|---|---|
| `EvidenceStore` (`Put`, `Get`, `Delete`) | Object storage is an external dependency; the constitution requires every one of them to be swappable in under five engineer-days, proved by a working second implementation | Filesystem-backed store used in tests and in the compose stack, alongside the S3-compatible one |

The ledger port is **not** extended. A goodwill payment is an ordinary transfer
with a different pair of accounts and a transfer kind; inventing a ledger
operation for it would put product vocabulary inside the money substrate.

## Project Structure

```
internal/cashback/claims/
  claims.go           - the domain type, states and transitions
  classify.go         - FR-105: attributable | adjustable | unevidenced
  handlers.go         - member endpoints
  ops.go              - operator endpoints
  evidence.go         - the EvidenceStore port
  evidence/fs/        - filesystem implementation
  evidence/s3/        - S3-compatible implementation
  store/              - sqlc-generated
  queries/            - the SQL
internal/platform/db/migrations/
  0033_cashback_claims.{up,down}.sql
  0034_cashback_claim_decisions.{up,down}.sql
web/src/
  lib/cashback/claims.ts       - typed client
  pages/[lang]/[place]/cashback/claim.astro
  pages/[lang]/[place]/cashback/claims/[reference].astro
  pages/ops/claims.astro
```

## Phasing

1. **Phase 1 — Governance.** Draft the amendment, get it ratified. Nothing
   else starts.
2. **Phase 2 — Schema and invariants.** Migrations, then the C-8/C-9/C-10
   rejection tests. The tests come before the handlers, because an invariant
   added after the first row exists is not an invariant.
3. **Phase 3 — Classification.** `classify.go` against fixtures covering all
   three cases and the boundaries between them. This is pure logic over rows
   that already exist, so it is testable before any endpoint is written.
4. **Phase 4 — Member path.** Submit, list, read. Evidence store behind its
   port, both implementations, conformance suite.
5. **Phase 5 — Operator path.** Queue, detail with the three record rows,
   the three remedies wired to the paths that already exist.
6. **Phase 6 — Frontend.** Member claim form and claim record; operator queue.
   Copy in the catalogues, no brand literals, `noindex`.
7. **Phase 7 — Accounting.** Goodwill as its own export line; the median
   answer time on the operator surface.

## Complexity Tracking

One deviation from the simplest thing, named as the constitution requires:

**Storing the classification and the three record rows on the decision
duplicates data that is derivable.** It is justified by SC-102: the audit
question is what was known at the time, and a derivation cannot answer it after
the underlying rows have moved on. The alternative — reconstructing the state
of the tracking log and the network feed at an arbitrary past instant — needs
temporal tables across three tables to answer one question about a few hundred
rows a year.

## Risks

- **Goodwill becomes the default.** It is the button that always works and
  always makes the member happy. Mitigated by making it visible: its own house
  account, its own export line, and a monthly total somebody reads. Not
  mitigated by a control that can be clicked through.
- **Evidence files are the first member-uploaded content in the product.**
  They bring malware scanning, content-type confusion and storage cost with
  them. The port keeps the blast radius to one package; Q11 keeps the
  retention question with the founder rather than in a default nobody chose.
- **The five-day promise is printed on the member's screen.** If it is not
  met, that is a broken public commitment rather than a missed internal
  target. Q12 exists so the founder decides which it is before it ships.
