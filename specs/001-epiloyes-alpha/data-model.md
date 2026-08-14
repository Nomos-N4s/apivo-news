# Data Model: epiloYES Alpha

**Feature**: [spec.md](spec.md) | **Date**: 2026-08-14

The schema's first migration (`internal/platform/db/migrations/0001_init.up.sql`)
already ships the invariant core, live on the foundations branch. This
document records what exists, what the alpha still needs (migration 0002),
and how each functional requirement maps to enforcement.

## Existing schema (0001) — the invariant core

| Table | Purpose | Invariant enforcement |
|---|---|---|
| `language` | BCP-47 primary subtags only (el, de, en) | CHECK rejects combined tags (`el-DE` unrepresentable) |
| `place` | Self-referencing hierarchy, country (alpha-2 format check), jurisdiction override | name/country CHECKs; no self-parenting |
| `source` | Licensed feed + usage rule + permission evidence | `usage_rule` defaults `extract_and_link`; `full_text` requires non-blank evidence; licence terms never blank (CHECKs) |
| `source_item` | Immutable retrieval evidence, incl. the feed's original title | Snapshot trigger writes licence/usage-rule/permission-evidence from the source row in the same transaction (callers cannot record false terms); `content_hash` is a DB-computed generated column over the body; NOT NULL + not-blank columns; UPDATE/DELETE/TRUNCATE triggers (I-2, I-3, I-4); dedupe UNIQUE (source, content_hash) |
| `translation` | Immutable model/prompt lineage; output stored as separate non-blank `headline` + `extract` (the extract-and-link shape) | NOT NULL lineage columns; not-blank output CHECKs; UPDATE/DELETE/TRUNCATE triggers (I-5) |
| `account` | Named people (approvers, readers) | display_name not blank; case-insensitive unique email |
| `consent` | Per-purpose dated records, never a boolean | consent guard: append-only history — no deletes, identity/grant frozen, revocation one-way; one active row per purpose (partial unique); revoke-after-grant CHECK |
| `article` | The approval itself | `approved_by` NOT NULL → account (I-1); exactly-one-origin CHECK; one article per origin (partial UNIQUE indexes — a double-approve race cannot create duplicates); article guard freezes identity/origin/approval/attribution after approval, publication is a one-way transition, DELETE/TRUNCATE blocked (I-5); attribution not blank |
| `article_place`, `reader_place` | Language/place independence, many-to-many | FKs + composite PKs |
| `domain_event` | Append-only audit stream | UPDATE/DELETE/TRUNCATE triggers |
| `article_provenance` (view) | I-5 in one query | source identification + retrieval-time snapshots (licence, usage rule, permission evidence) + model + prompt + approver — historical legal basis never read from the mutable source row |
| `is_entitled(uuid, text)` | The single entitlement gate | all access rules live here |

## Planned deltas (migration 0002) — required by the clarified spec

All additions preserve the immutability guarantees: new columns on
immutable tables are written at insert time only; nothing loosens a
trigger or constraint.

1. **Editor role, DB-enforced approval authority** (FR-007 hardening)
   - `account.role text NOT NULL DEFAULT 'reader' CHECK (role IN ('reader','editor'))`
   - `BEFORE INSERT ON article` trigger: raise unless `approved_by`
     references an account with `role = 'editor'` — "named human approver"
     tightens to "named editor", enforced by the database like I-1 itself.
   - `is_entitled` extended: editorial actions require the editor role.

2. **Withdrawal, record-preserving** (FR-016)
   - `article.withdrawn_at timestamptz`, `article.withdrawn_by uuid REFERENCES account`,
     `article.withdrawal_reason text`
   - CHECKs: the three are all-null or all-set; `withdrawn_at >= published_at`;
     withdrawal only for published articles. Published-and-visible =
     `published_at IS NOT NULL AND withdrawn_at IS NULL`.
   - The article guard function is extended (CREATE OR REPLACE) to allow
     exactly one more one-way transition: the withdrawal columns moving
     from null to set, together, once. Everything else stays frozen.
   - The one-article-per-origin unique indexes are rebuilt as
     `... where withdrawn_at is null`, so a withdrawn article's origin can
     be republished (correction flow) while active duplicates stay
     impossible.
   - Withdrawal writes a `domain_event` (`article.withdrawn`, payload: who,
     why, article id) in the same transaction.
   - `article_provenance` view gains the withdrawal columns (audit sees
     full history; reader queries exclude withdrawn).

3. **Translation cost lineage** (FR-006)
   - `translation.cost_microusd bigint NOT NULL DEFAULT 0` — cost recorded
     at generation into the immutable row.
   - `translation_spend (month date PRIMARY KEY, spent_microusd bigint NOT NULL)`
     — monthly ledger updated in the same transaction as each translation
     insert. Caps themselves are configuration (per-article ceiling,
     monthly cap); the translation module refuses work once the ledger
     reaches the cap and emits a `pipeline.halted` domain event.

4. **Reader-facing place addressing** (FR-009, US1)
   - `place.slug text UNIQUE` (not blank) — stable, human-readable
     addressing for locale-scoped pages (e.g. `munich`).
   - Seed data: Munich → Bavaria → Germany hierarchy; Greece (national).
     Seeds are reference data and live in the migration, like `language`.

5. **Source operations** (US2 edge cases)
   - `source.active boolean NOT NULL DEFAULT true` — pausing a feed
     without deleting anything.

## FR → enforcement traceability

| FR | Enforcement point |
|---|---|
| FR-001 | `source.url` is the only ingestion origin; no other fetch path exists in `internal/ingestion` |
| FR-002 | `source_item` NOT NULL/not-blank columns + snapshot trigger + DB-computed hash, single-transaction write (0001) |
| FR-003 | immutability triggers (0001) |
| FR-004 | usage-rule default + evidence CHECK + retrieval-time rule snapshot (0001) |
| FR-005 | `translation.headline`/`.extract` non-blank columns + lineage columns + immutability (0001); extract-only is additionally an ingestion/translation module rule verified by tests |
| FR-006 | `cost_microusd` + `translation_spend` ledger (0002) + config caps |
| FR-007 | `approved_by NOT NULL` (0001) + editor-role trigger (0002) |
| FR-008 | `attribution_block` not blank (0001); rendering asserted by frontend tests |
| FR-009 | separate `language` and `place` axes (0001) + `place.slug` (0002) |
| FR-010 | `article_provenance` view (0001, extended 0002) |
| FR-011 | `account`, `consent` (0001) |
| FR-012 | `domain_event` append-only (0001); emitting events is a module obligation verified by tests |
| FR-013 | one frontend middleware (robots + X-Robots-Tag); the Go API is non-publicly-routable and stamps `X-Robots-Tag` on every response (0001, platform http); no per-route logic anywhere |
| FR-014 | UNIQUE (source_id, content_hash) (0001) |
| FR-015 | route layer mounts only el and de, verified by frontend tests. (`language` seeds el, de and en — en exists for future use and is not reachable through any route) |
| FR-016 | withdrawal columns + CHECKs + same-tx domain event (0002) |

## State transitions

- **Retrieved item**: created → (terminal; immutable evidence)
- **Translation**: created with cost → (terminal; immutable lineage);
  re-translation = new row
- **Article**: approved (row exists) → published (`published_at` set,
  possibly at approval) → withdrawn (`withdrawn_at` set; terminal, record
  preserved). No transition ever deletes or rewrites history.
- **Consent**: granted (row) → revoked (`revoked_at` set) → re-granted
  (new row). History append-only by construction.
