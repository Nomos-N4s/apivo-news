# Feature Specification: epiloYES Alpha — Licensed, Translated Local News for the Greek Diaspora

**Feature Branch**: `001-epiloyes-alpha`

**Created**: 2026-08-14

**Status**: Draft

**Input**: User description: "v1.0.0-alpha of epiloYES: Greek and German; Munich as reader locale; Greek national and Munich local RSS/Atom feeds as sources; no scraping; text only; translated headline and extract linking back to the source, not full-text translation; human approval on every item before publish; full provenance per the constitution's invariants; reader front page and article pages, locale-scoped, attribution rendered; registration and consent capture in the schema, UI only if time allows."

## Clarifications

### Session 2026-08-14

- Q: Indexing posture (§9.1) — how should the alpha treat crawlers? →
  A: Keep blocking everything (search engines, AI-training crawlers,
  archives) at one edge enforcement point for the whole alpha. Founder
  decision; revisiting it is a new founder decision, not a default.
- Q: Data retention periods (§9.2)? → A: Deferred by the founder; no
  automated deletion ships in the alpha. A retention schedule must be
  decided before public launch — carried as a founder-blocked backlog
  item.
- Q: LLM translation provider (§9.3)? → A: The plan carries the adapter
  interface, cost controls, and a priced shortlist of 2–3 candidates;
  the founder picks at plan review. No provider is wired until then.
- Q: Withdrawal semantics for a published article? → A: Unpublish keeps
  the record: withdrawal ends publication, while the article row, the
  approval record and the retrieved evidence remain; an audit event
  records who withdrew it and why. Nothing is ever deleted.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Munich reader, in Greek (Priority: P1)

A Greek speaker living in Munich opens the site and reads a front page of
current news in Greek: local Munich items alongside Greek national items.
Each item shows a translated headline and short extract, names the original
publisher, and links to the original article. Opening an item shows the same
content on its own page with full attribution.

**Why this priority**: This is the entire product thesis — language and
place as independent axes. Without it there is no product; everything else
exists to feed this page lawfully.

**Independent Test**: Seed a handful of approved items covering both a
Munich place and Greece, open the front page as an anonymous visitor, and
verify Greek-language presentation, place scoping, attribution and working
outbound links.

**Acceptance Scenarios**:

1. **Given** approved items exist for Munich and for Greece, **When** a
   visitor opens the front page in Greek, **Then** they see both local and
   national items in Greek, newest first, each with publisher attribution
   and a link to the original.
2. **Given** an item's article page, **When** the visitor follows the
   source link, **Then** they land on the original publisher's page.
3. **Given** no approved items exist for a followed place, **When** the
   front page loads, **Then** the page states plainly that nothing is
   published yet for that place — no errors, no filler content.

---

### User Story 2 - Retrieval with evidence (Priority: P2)

The operator configures a licensed RSS/Atom feed as a source. The system
polls it, stores each new item exactly as retrieved together with where it
came from, when it was retrieved, who wrote it (when stated), a content
fingerprint, and the licence terms that applied at that moment. Retrieved
items can never be altered or deleted afterwards.

**Why this priority**: Legal defensibility depends on evidence captured at
retrieval. Publishing without this is a liability, so it precedes
translation and editorial work.

**Independent Test**: Point the system at a live feed, verify each item is
stored with complete provenance in the same operation as the content, and
verify stored items reject any subsequent change.

**Acceptance Scenarios**:

1. **Given** a configured source, **When** a new feed item appears,
   **Then** a retrieved item is recorded with origin link, retrieval time,
   content fingerprint, full retrieved text and the licence snapshot — all
   present, or nothing is recorded at all.
2. **Given** a stored retrieved item, **When** any change or removal is
   attempted, **Then** the system refuses and the original record stands.
3. **Given** a feed serving an item already retrieved (same content),
   **When** polling repeats, **Then** no duplicate record is created.
4. **Given** a new source is added, **When** its record is created,
   **Then** its usage rule is extract-and-link; a full-text rule cannot be
   set without recorded written permission.

---

### User Story 3 - Machine translation with lineage (Priority: P3)

For each retrieved item, the system produces a translated headline and
short extract in the reader's language — never a full-text translation.
Every translation records which model produced it, under which prompt
version, and when.

**Why this priority**: Translation makes the content readable by the
audience, but only after retrieval evidence exists; its lineage is part of
the provenance chain.

**Independent Test**: Feed a retrieved item through translation and verify
the output is headline-and-extract only, linked to its retrieved item, with
model, prompt version and generation time recorded and unchangeable.

**Acceptance Scenarios**:

1. **Given** a retrieved Greek item, **When** translation to German runs,
   **Then** a translation record exists holding headline and extract, the
   model name, prompt version and generation time.
2. **Given** a recorded translation, **When** any change is attempted,
   **Then** the system refuses; a re-translation creates a new record.
3. **Given** the configured monthly translation budget is exhausted,
   **When** the next translation is requested, **Then** the pipeline halts
   and reports, rather than overspending.
4. **Given** a per-article cost ceiling, **When** a single item would
   exceed it, **Then** that item is skipped and flagged, not translated at
   any cost.

---

### User Story 4 - Named human approval (Priority: P4)

An editor reviews translated items in a queue and approves or rejects each
one. Only approval by a named person creates a publishable article, and the
article records who approved it and when. There is no path to publication
that bypasses a human.

**Why this priority**: The approval gate is one of the two things that can
never be cut. It converts retrieved evidence and translations into
something the business is willing to stand behind.

**Independent Test**: Attempt to create an article without an approver and
verify the system makes it impossible; approve an item as a named editor
and verify the article records that identity.

**Acceptance Scenarios**:

1. **Given** a translated item in the review queue, **When** a named editor
   approves it, **Then** an article exists recording that editor and the
   approval time, and it can be published.
2. **Given** any attempt to create an article without a named approver,
   **Then** the system rejects it outright.
3. **Given** an item the editor rejects, **Then** no article is created
   and the retrieved item remains untouched as evidence.
4. **Given** a published article that must be withdrawn, **When** an
   editor withdraws it, **Then** it stops appearing on the site while
   the article, its approval record and the retrieved evidence remain,
   and an audit event records who withdrew it and why.

---

### User Story 5 - Five-minute provenance audit (Priority: P5)

Someone with audit access (founder, lawyer, or a publisher inquiring) picks
any published item and, within five minutes, obtains: the source it came
from, the licence terms that applied at retrieval, the model and prompt
version that translated it, and the named person who approved it.

**Why this priority**: This is the promise that keeps the legal exposure
bounded. It is cheap once the chain exists, but it must be demonstrated,
not assumed.

**Independent Test**: Pick published items at random and time the trace;
every element must be recoverable in a single lookup.

**Acceptance Scenarios**:

1. **Given** any published article, **When** the audit lookup runs,
   **Then** it returns source identity, feed link, licence snapshot at
   retrieval, translation model and prompt version (when translated), and
   the approver's name — in one query, well inside five minutes.

---

### User Story 6 - Registration and consent capture (Priority: P6)

A reader can create an account and follow places (for example Munich and
their home region in Greece). Every consent the reader gives — for any
purpose — is recorded as its own dated record, revocable, with history
preserved. If time runs short, the visible registration flow is cut while
the underlying account and consent capability remains.

**Why this priority**: First on the cut list (§8), but the capability
cannot be retrofitted without a user migration, so it exists from the
first schema even if no UI ships.

**Independent Test**: Create an account, grant and revoke a consent, grant
it again, and verify the full history is preserved with no single yes/no
flag anywhere.

**Acceptance Scenarios**:

1. **Given** a new reader, **When** they register, **Then** an account
   exists and they can follow multiple places at once.
2. **Given** a granted consent, **When** the reader revokes it, **Then**
   the revocation is dated and a later re-grant creates a new record,
   preserving history.

---

### Edge Cases

- Feed unreachable or serving malformed content: the poll fails visibly
  for the operator; nothing partial is stored.
- Feed omits author or publication date: the item is still retrieved;
  retrieval time and licence snapshot are always present, missing fields
  stay empty rather than invented.
- Publisher changes licence terms: items retrieved before the change keep
  the old snapshot; items after carry the new one. The defence rests on
  what applied at the time.
- Translation provider outage: items queue untranslated; nothing publishes
  without the editor seeing what state it is in.
- Same story on two feeds: each retrieval is separate evidence with its
  own source and licence; deduplication applies within a source only.
- An approved article needs to be withdrawn: see FR-016 — publication
  ends, every record remains, and the withdrawal itself is audited.
- A reader in Munich also follows a place in Greece: both feeds of content
  appear; place following is many-to-many by design.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The system MUST ingest content only from explicitly
  configured RSS/Atom feeds. No scraping of any kind.
- **FR-002**: The system MUST record, in the same operation as the content
  itself: origin link, retrieval time, content fingerprint, full retrieved
  text, licence terms as they applied at retrieval, and author and
  publication date when the feed provides them. A retrieved item without
  this evidence MUST be impossible to create.
- **FR-003**: Retrieved items MUST be immutable: no update, no deletion,
  no bulk clearing. Corrections happen by new records.
- **FR-004**: Every new source MUST default to extract-and-link usage. A
  full-text usage rule MUST be impossible to record without written
  permission evidence attached.
- **FR-005**: Translations MUST be limited to headline and extract, and
  MUST record model, prompt version and generation time. Translation
  records MUST be immutable; re-translation creates a new record.
- **FR-006**: Translation spending MUST respect a per-article ceiling and
  a monthly cap; reaching the cap halts the pipeline rather than
  overspending.
- **FR-007**: Publication without approval by a named human MUST be
  impossible. The approval record (who, when) is part of the article
  itself.
- **FR-008**: Every published item MUST render attribution naming the
  original publisher and MUST link to the original article.
- **FR-009**: Language and place MUST remain independent axes: a reader
  chooses a reading language and follows one or more places; the front
  page is scoped by both. No combined language-place locale value may
  exist anywhere.
- **FR-010**: For any published item, the system MUST answer — in a single
  query — its source, licence snapshot at retrieval, translation model and
  prompt version (when translated), and named approver.
- **FR-011**: Accounts and per-purpose consent records MUST exist from the
  first schema. Consent is always a dated record per purpose with
  grant/revoke history — never a boolean flag.
- **FR-012**: Significant domain events (retrieval, translation, approval,
  publication) MUST be recorded in an append-only audit stream.
- **FR-013**: For the whole alpha, the site MUST block all automated
  crawling and indexing (search engines, AI-training crawlers, archives),
  enforced in one place at the edge — never per-route. (Founder decision,
  2026-08-14; revisiting it is a new founder decision.)
- **FR-014**: Repeated retrieval of identical content from the same source
  MUST NOT create duplicate records.
- **FR-015**: All reader-facing pages MUST be served with content
  negotiation limited to the alpha languages (Greek, German) and MUST
  render correctly in both.
- **FR-016**: An editor MUST be able to withdraw a published article:
  withdrawal ends publication while preserving the article, its approval
  record and the retrieved evidence, and records who withdrew it and why
  in the append-only audit stream. Deletion is never part of withdrawal.

### Key Entities

- **Source**: A licensed feed — name, feed link, jurisdiction, licence
  terms on record, usage rule (extract-and-link by default), permission
  evidence when upgraded.
- **Retrieved Item**: Immutable evidence of one retrieval — origin, times,
  fingerprint, full text, licence snapshot. Belongs to a Source.
- **Translation**: Immutable lineage — the translated headline/extract,
  target language, model, prompt version, generation time. Belongs to a
  Retrieved Item.
- **Article**: The published unit — exactly one origin (a Translation, or
  a Retrieved Item directly), the named approver and approval time,
  publication time, attribution text.
- **Approver (Account)**: A named person. Also the reader identity for
  registration; holds consent records and followed places.
- **Language / Place**: Independent axes. Places form a hierarchy (Munich
  → Bavaria → Germany) with optional jurisdiction override; articles and
  readers relate to places many-to-many.
- **Consent**: A dated per-purpose record with optional revocation date;
  history is never overwritten.
- **Audit Event**: Append-only record of what happened and when.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A person in Munich opens the site and reads local and Greek
  news in Greek; every visible item names its publisher and links back to
  the original. (The alpha's definition of done.)
- **SC-002**: For 100% of published items, an auditor recovers source,
  licence-at-retrieval, translation lineage and approver in under five
  minutes — demonstrated on randomly picked items.
- **SC-003**: 100% of published items have a named human approver; the
  count of articles without one is provably zero and cannot grow.
- **SC-004**: 100% of retrieved items carry a complete licence snapshot
  from the moment of retrieval; none can be altered afterwards.
- **SC-005**: Zero published items exceed their source's usage rule; no
  full text appears for any source without recorded written permission.
- **SC-006**: Translation spend never exceeds the configured monthly cap;
  when the cap is reached the pipeline stops within one item.
- **SC-007**: The front page renders readable content in under two seconds
  on an ordinary connection.
- **SC-008**: A visitor can reach any published article's original source
  in one click from the article page.

## Assumptions

- Founder decisions recorded in Clarifications (2026-08-14): crawler
  blocking is the decided alpha posture; retention periods are explicitly
  deferred (no automated deletion in the alpha, decision required before
  public launch); the translation provider is chosen at plan review from
  a priced shortlist. Per-source usage rules keep their conservative
  default (extract-and-link until founder review).
- Licensed feeds exist for Greek national and Munich local news with
  usable headlines and summaries; where summaries are absent, the extract
  derives from the retrieved text within the extract-and-link rule.
- Feeds may omit author and publication date; provenance stores what the
  feed provides and never invents missing values.
- Alpha readership is modest (hundreds of readers); ordinary web
  performance expectations apply, no special scale targets.
- The German reading experience uses the same pipeline as Greek; Greek in
  Munich is the flagship journey and takes priority in polish decisions.
- Registration UI is the first thing cut under time pressure; account and
  consent capability ships in the schema regardless.
- Editorial staff in the alpha is a small, trusted group of named
  individuals; role/permission granularity beyond "named approver" is out
  of scope.
