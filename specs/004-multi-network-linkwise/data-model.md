# Data Model Deltas: Many Affiliate Networks

**Feature**: [spec.md](spec.md) | **Plan**: [plan.md](plan.md) | **Date**: 2026-09-03

This file describes **only what changes**. The cashback data model is
`002/data-model.md`; everything not listed here is unchanged and correct as
it stands.

That is the headline finding. The schema was designed multi-network from the
start — `cashback.network` is a *table* with a typed, human-readable primary
key; `cashback.merchant_network` is one route per network per retailer, and
its own comment says so:

> *'One route to a retailer through one network. The same retailer is
> commonly live on several networks at once with different rates and
> different reliability, so everything per-network lives here…'*
> — `0011_cashback_catalogue.up.sql:161`

**No new table. No new entity. No column dropped.** Four migrations,
`0033`–`0036`, each carrying one rule that was an application-level
assumption and becomes a database-level fact (Principle VIII). `0032` is the
current head.

---

## 0033 — One click backs at most one credit

### The defect

`cashback.entry` already carries *one report backs one credit*:

```sql
create unique index entry_one_per_report
    on cashback.entry (network_transaction_id)
 where reversal_of_id is null;      -- 0032
```

The click has no such rule. It has an ordinary index:

```sql
create index entry_click_id_idx on cashback.entry (click_id)
 where click_id is not null;        -- 0013:105
```

With one network that is harmless, because a click reference is unique
(`click_ref_unique`) and only one network ever echoes it back. With two, a
reference reported by **both** networks matches the same click twice, and
each report is a different `network_transaction_id` — so
`entry_one_per_report` is satisfied by each, and the member is credited
**twice for one purchase**.

### The change

```sql
drop index cashback.entry_click_id_idx;

create unique index entry_click_id_idx
    on cashback.entry (click_id)
 where click_id is not null and reversal_of_id is null;
```

The name is kept, exactly as `0032` kept `entry_one_per_report`: a unique
index reports its own name on refusal, and code that recognises the refusal
recognises it by name.

**Why reversals are excluded**: the same reason `0032` excludes them. A
reversal cites the click of the credit it undoes, and must be allowed to. A
reversal being at most one per credit is already `entry_reversed_at_most_once`.

**Why the `is not null` predicate survives**: Postgres treats NULLs as
distinct in a unique index anyway, so it is not needed for correctness — it
is kept because an operator-attributed entry has no click (`0013`'s own
comment) and the predicate says so in the schema rather than in a comment.

### Test

Real Postgres. Insert a credit citing a click; insert a second, non-reversal
credit citing the same click through a different `network_transaction_id`;
assert the **database** refuses with SQLSTATE `23505` naming
`entry_click_id_idx`.

---

## 0034 — A published route must be alive *and* attributable

### The defect

`merchant_network_one_preferred` (`0011:181-183`) guarantees **at most one**
preferred route per retailer. It does not guarantee the preferred route is
usable:

```sql
create unique index merchant_network_one_preferred
    on cashback.merchant_network (merchant_id)
 where preferred;
```

`status` may be `active`, `paused` or `left_network`. So a route that has
**left the network** may hold the published slot while a live route sits
beside it, and — because the index only forbids a second — nothing takes
over. Combined with the second finding (nothing writes `preferred` after the
insert that created the row), the first network to import a retailer owns
its published rate permanently, including after it stops carrying that
retailer at all.

### The change

```sql
-- Contract rule 10 (contracts/ports.md): a route states whether it can
-- carry a click reference. Default true because every route that exists
-- today can - Awin's click_ref_param is network-wide - and false is the
-- value an importer must set deliberately.
alter table cashback.merchant_network
    add column can_attribute boolean not null default true;

alter table cashback.merchant_network
    add constraint merchant_network_preferred_is_publishable
    check (not preferred or (status = 'active' and can_attribute));

comment on column cashback.merchant_network.can_attribute is
    'Whether a click through this route carries our click reference back to us. False is not broken: the member clicks, buys, and the network pays the publisher - it just cannot say whose purchase it was. A route that cannot be attributed can never be the published one, because publishing a rate we cannot honour is worse than publishing nothing.';
comment on constraint merchant_network_preferred_is_publishable
    on cashback.merchant_network is
    'The published route must be usable: active, and able to carry a click reference. Demotion is then forced to be an explicit act with a successor, rather than a dead or unattributable row quietly outranking a live one.';
```

The index is unchanged; the constraint and the column are what it was
missing.

**Why `can_attribute` defaults to true**: every route that exists today can
be attributed — Awin's `click_ref_param` is a network-wide fact, carried on
`cashback.network` itself. So `true` is the truth for the whole existing
table and needs no backfill, and `false` is a value an importer sets
deliberately, from a fact its adapter learned. A default of `false` would
unpublish the entire catalogue on migration.

### What is deliberately *not* a constraint

Two neighbouring rules cannot be expressed as a check, and pretending
otherwise would be worse than stating it:

| Rule | Why not a constraint | Where it lives instead |
|---|---|---|
| A retailer with an active route MUST have a preferred one | Cross-row assertion over a set — a check sees one row | The demotion path, plus an operator query "published nothing, routes available" (FR-100) |
| A preferred route's **network** must be active | Cross-table: `network.active` is another table | The same demotion path, driven by the network being deactivated |
| A retailer whose routes are **all** unattributable must be visibly so | Cross-row again — one row cannot see its siblings | The operator listing behind SC-026 |

Both are covered by tests against real Postgres asserting the *behaviour*,
and by an operator listing that makes a violation visible rather than
silent. A trigger was rejected: it would validate writes and say nothing
about rows already there, which is precisely the state this defect creates.

### Test

Real Postgres. Attempt to prefer a `left_network` route; assert refusal by
SQLSTATE `23514` naming `merchant_network_preferred_is_publishable`. Repeat
with an `active` route whose `can_attribute` is false — that is SC-026, and
it is the case the constraint exists for. Then: mark the preferred route
`left_network` and assert a surviving active route holds the slot
afterwards.

---

## 0035 — A click carries the network that issued it

### The defect

Attribution must ask *which network issued this click*. Today that is three
tables:

```text
click.offer_id → offer.merchant_network_id → merchant_network.network_id
```

FR-096 makes the answer a predicate on the hot attribution path, and the
current shape makes that predicate a join.

### Why the denormalisation needs keys, not comments

`0011` refused this denormalisation once already, and said why:

> *'…the network it is sourced from follows from the route
> (`merchant_network`) rather than being repeated here where the two could
> disagree.'* — `0011:261`, on `cashback.offer`

That objection is correct and applies here unchanged. It is answered not by
overruling it but by making disagreement **unrepresentable** — the technique
`0012` already used for exactly this purpose:

> *'Redundant against the primary key on its own; it exists so 0013 can
> carry the ownership rule in a foreign key rather than in a trigger.'*
> — `0012:50-54`, on `click_id_account_unique`

### The change

```sql
-- The two keys the composite foreign keys join on. Each is redundant
-- against its own primary key; each exists so the value below can be
-- pinned by a key rather than trusted.
alter table cashback.offer
    add constraint offer_id_merchant_network_unique unique (id, merchant_network_id);

alter table cashback.merchant_network
    add constraint merchant_network_id_network_unique unique (id, network_id);

alter table cashback.click
    add column merchant_network_id uuid,
    add column network_id text;

-- Backfill from the join the columns replace, then close the door.
update cashback.click c
   set merchant_network_id = o.merchant_network_id,
       network_id          = mn.network_id
  from cashback.offer o
  join cashback.merchant_network mn on mn.id = o.merchant_network_id
 where o.id = c.offer_id;

alter table cashback.click
    alter column merchant_network_id set not null,
    alter column network_id set not null,
    add constraint click_route_matches_offer
        foreign key (offer_id, merchant_network_id)
        references cashback.offer (id, merchant_network_id),
    add constraint click_network_matches_route
        foreign key (merchant_network_id, network_id)
        references cashback.merchant_network (id, network_id);

create index click_network_id_idx on cashback.click (network_id);
```

Two columns, not one. `merchant_network_id` is the route the member actually
clicked — the thing the rate snapshot came from — and it is the only value
that can chain the network back to the offer by key. `network_id` is the
attribution key, and with the second foreign key it cannot name a network
the route does not belong to.

`cashback.click` is append-only (C-3), so neither column can drift after the
insert that set it.

### Rejected alternatives

| Alternative | Rejected because |
|---|---|
| One column, `network_id`, no keys | Nothing stops it disagreeing with the offer's route — exactly the objection `0011:261` raised |
| A trigger validating the pair | Validates writes, says nothing about rows already present |
| Add `network_id` to `cashback.offer` instead | Overrules `0011:261` at the table it was written about, and puts the value one level further from where it is needed |
| Leave it joined | FR-096 becomes a performance argument rather than a rule |

### Test

Real Postgres. Insert a click whose `network_id` names a network its route
does not belong to; assert refusal by SQLSTATE `23503` naming
`click_network_matches_route`. Assert the backfill leaves no null.

---

## 0036 — A member's entry is in a currency they can be paid in

### The defect

An entry's currency is whatever the network reported
(`earnings/lifecycle.go:250-256`). Withdrawal is deployment-wide
single-currency: `earnings.Confirmed` selects only entries in the requested
currency (`reserve.go:107-114`), and the request's currency must equal the
payout threshold's or it is refused (`payout/withdrawal.go:306-309`,
`ErrCurrencyNotPaid`).

So an entry in any other currency is **credited, confirmed, counted in the
member's wallet, and unwithdrawable**. Not refused, not queued, not flagged.
With one network and one market this could not arise. With two it is one
configuration away, and its symptom is a member looking at money that does
not move.

### The change

The deployment's payout currency is configuration and cannot be a check
constraint. But the **member's own** currency is a column —
`cashback.participation.default_currency`, recorded at the moment they
accepted terms — and an entry already belongs to an account. So the rule
that matters is expressible exactly:

```sql
-- The key the composite foreign key joins on. Redundant against
-- participation's own primary key; it exists so the rule below can be a
-- key rather than a trigger, which is the technique 0012 and 0035 both use.
alter table cashback.participation
    add constraint participation_account_currency_unique
    unique (account_id, default_currency);

alter table cashback.entry
    add constraint entry_currency_is_the_members
    foreign key (account_id, currency)
    references cashback.participation (account_id, default_currency);

comment on constraint entry_currency_is_the_members on cashback.entry is
    'A member is only ever credited in the currency their participation is denominated in - the one a withdrawal can actually reserve and pay out. Without this, a network reporting in another currency produces a balance the member can see and can never withdraw, and nothing anywhere says so.';
```

Two things follow, and both are wanted:

- **An entry in a foreign currency is unrepresentable.** The insert is
  refused, the crediting path recognises the refusal by name, and the report
  is queued for an operator naming the currency (FR-109) — the same shape as
  an unattributable report.
- **A member's currency cannot change while they hold a balance in the old
  one.** `0017` lets a re-joining member restate `default_currency`; that
  update is now refused while entries reference the old value. That is the
  correct answer to a question nobody had asked: their money is denominated
  in what they were credited in.

### The consequence this constraint forces, stated rather than smuggled

Joining through `participation` makes crediting **require** an opt-in. That
is correct — an entry for a member who never accepted terms was always
wrong, and nothing said so — but today **nothing stops it**: the click-out
path has no participation check anywhere
(`internal/cashback/clickout/` contains no reference to it), so a signed-in
member who never opted in can click out and be credited.

Left alone, this constraint would turn that into the wrong failure: the
member clicks, buys, and their report is refused at crediting. So the
click-out gate (FR-110) lands **before** this migration, not after. It is a
widening of this feature, forced by the choice above, and it is a small one:
FR-002 already says participation is an explicit opt-in, and an opt-in that
a credit can precede is not one.

### And the earlier refusal

The constraint catches a report that arrived. FR-108 asks for the refusal
one step earlier, where a person can act on it, so `network_account` records
what its network reports in:

```sql
alter table cashback.network_account
    add column reports_currency char(3)
        constraint network_account_reports_currency_iso4217_format
            check (reports_currency is null or reports_currency ~ '^[A-Z]{3}$');

comment on column cashback.network_account.reports_currency is
    'The currency this publisher account''s network reports commission in, as its adapter declares it. Null means nobody has established it yet - which is itself the state that produces an unwithdrawable balance, so an operator listing shows it as such rather than as blank.';
```

Nullable on purpose. An account connected before its adapter has a recording
has nothing honest to declare, and a default would be a guess about money.
`connect-network` refuses a **declared** currency this deployment cannot pay
out (FR-108); a null is reported, not refused.

### Test

Real Postgres. Insert an entry whose currency differs from the member's
participation currency; assert SQLSTATE `23503` naming
`entry_currency_is_the_members`. Then attempt to restate a participating
member's `default_currency` while an entry references the old one; assert
the same. And connect an account declaring a currency other than the payout
threshold's; assert the refusal names both currencies.

---

## Read-path consequences

| Query | Change |
|---|---|
| `GetClickByRef` | Takes the reporting network; returns the click only if `click.network_id` matches. A reference belonging to another network's click returns **no row**, and the caller queues it as unattributed with a distinguishable reason (FR-098) |
| `ReportsAwaitingCredit` | Unchanged. It excludes by `(network_id, external_id)` and correctly says nothing about clicks |
| Unattributed queue | Gains a reason discriminating "no click has this reference" from "that reference belongs to another network" (FR-098), and a network filter (FR-102) |
| Catalogue read | Unchanged. It already reads through the preferred route |

## Generated code

`sqlc` regenerates `internal/cashback/clickout/store` and
`internal/cashback/catalogue/store`; the drift check in CI is what proves
the regeneration happened. `supabase gen types` covers the `cashback` schema
already and needs no new configuration.
