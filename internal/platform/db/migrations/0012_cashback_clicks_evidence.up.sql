-- 0012_cashback_clicks_evidence: the two tables a member's money rests on.
--
-- This migration is to cashback what source_item is to news. Both tables
-- here are EVIDENCE, and evidence that can be edited is not evidence:
--
--   C-3  Network transaction records and click records reject UPDATE,
--        DELETE and TRUNCATE. A status change is a new superseding record,
--        never an edit. Enforced with the existing raise_immutable().
--
-- Immutability forces the shape of everything else in this file. A
-- re-reported transaction cannot mark the previous row superseded, because
-- that would be an update - so the chain is built the other way round, and
-- "exactly one current row" is derived rather than stamped:
--
--   * exactly one ROOT per transaction   (partial unique index)
--   * no FORKS in the chain              (unique on supersedes_id)
--   * every link stays within one key    (insert guard)
--
-- One root plus no forks means one path, and one path has exactly one tip.
-- The tip is the current row, and nothing had to be rewritten to say so.

------------------------------------------------------------------------------
-- Clicks: the attribution evidence, and the rate that governs the credit.
------------------------------------------------------------------------------

create table cashback.click (
    id uuid primary key default gen_random_uuid(),
    click_ref text not null
        constraint click_ref_unique unique
        -- The database cannot measure entropy, so it enforces what 128 bits
        -- of it needs once encoded: at least 22 URL-safe characters (22
        -- base64url characters carry 132 bits; a 32-character hex string
        -- also satisfies it). A short or punctuated reference is refused
        -- here rather than discovered as a guessable one later (FR-020).
        constraint click_ref_url_safe_and_long_enough
            check (click_ref ~ '^[A-Za-z0-9_-]{22,}$'),
    -- FR-023: an anonymous click can never later be credited to an account,
    -- and the cheapest way to guarantee that is to make an anonymous click
    -- unrepresentable in the first place.
    account_id uuid not null references public.account (id),
    offer_id uuid not null references cashback.offer (id),
    clicked_at timestamptz not null default now(),
    rate_snapshot jsonb not null,
    member_share_bps_snapshot integer not null
        constraint click_member_share_bps_range
            check (member_share_bps_snapshot between 0 and 10000),
    context_digest text
        constraint click_context_digest_not_blank
            check (context_digest is null or btrim(context_digest) <> ''),
    -- The pair the entry table joins on: a credit may only cite a click
    -- that belongs to the member being credited. Redundant against the
    -- primary key on its own; it exists so 0013 can carry the ownership
    -- rule in a foreign key rather than in a trigger.
    constraint click_id_account_unique unique (id, account_id)
);

comment on table cashback.click is
    'APPEND ONLY (C-3). The tracked redirect: who clicked, which band, when, and the rate as published at that moment. Every credit that follows is evidenced by a row here.';
comment on column cashback.click.click_ref is
    'The unguessable reference passed to the network in its own click-reference parameter (FR-020, FR-021), and the value every reported transaction is matched back on. Unique, URL-safe, and at least as long as 128 bits of entropy requires.';
comment on column cashback.click.account_id is
    'The member who clicked. NOT NULL by design (FR-023): an anonymous click is unrepresentable, so it can never later be credited to an account.';
comment on column cashback.click.rate_snapshot is
    'The whole rate band as published at click time (FR-013). This, not the offer row as it stands today, is what governs the credit - a rate change after the click never reaches back.';
comment on column cashback.click.member_share_bps_snapshot is
    'The member share in basis points at click time, snapshotted beside the band for the same reason (FR-013).';
comment on column cashback.click.context_digest is
    'A privacy-minimised device or context digest, sufficient for abuse rules and no more (FR-022). Never a raw fingerprint, never an address.';

create index click_account_clicked_at_idx on cashback.click (account_id, clicked_at desc);
create index click_offer_id_idx on cashback.click (offer_id);

create trigger click_immutable
    before update or delete on cashback.click
    for each row execute function public.raise_immutable();
create trigger click_no_truncate
    before truncate on cashback.click
    for each statement execute function public.raise_immutable();

------------------------------------------------------------------------------
-- Network transactions: immutable evidence of what a network reported.
------------------------------------------------------------------------------

create table cashback.network_transaction (
    id uuid primary key default gen_random_uuid(),
    network_id text not null references cashback.network (id),
    network_account_id uuid not null references cashback.network_account (id),
    external_id text not null
        constraint network_transaction_external_id_not_blank
            check (btrim(external_id) <> ''),
    -- Null when the network reported no click reference at all. The row is
    -- still stored: an unattributed transaction is evidence too (FR-034).
    --
    -- Null and blank must not both be sayable. The digest below folds a
    -- null reference to the empty string, so a blank one would fingerprint
    -- identically to no reference at all - two reports the schema would
    -- then call the same report. Worse, `where click_ref is not null`
    -- counts a blank as attribution present, so an unattributed
    -- transaction could sit in the attributed index carrying nothing.
    -- Rejected rather than silently normalised: this row is evidence of
    -- what a network said, and quietly rewriting it is not this table's
    -- job.
    click_ref text
        constraint network_transaction_click_ref_not_blank
            check (click_ref is null or btrim(click_ref) <> ''),
    status_raw text not null
        constraint network_transaction_status_raw_not_blank
            check (btrim(status_raw) <> ''),
    status text not null
        constraint network_transaction_status_known
            check (status in ('pending', 'confirmed', 'declined', 'reversed')),
    sale_amount_minor bigint not null,
    commission_minor bigint not null,
    currency char(3) not null
        constraint network_transaction_currency_iso4217_format
            check (currency ~ '^[A-Z]{3}$'),
    transacted_at timestamptz not null,
    retrieved_at timestamptz not null default now(),
    query_window_start timestamptz not null,
    query_window_end timestamptz not null,
    raw_payload jsonb not null,
    content_digest text not null,
    supersedes_id uuid references cashback.network_transaction (id),
    constraint network_transaction_query_window_ordered
        check (query_window_end >= query_window_start),
    -- An unchanged re-report is the same row, and the database says so
    -- rather than the poller remembering to check (US2 scenario 3).
    constraint network_transaction_unique_report
        unique (network_id, external_id, content_digest),
    -- No forks: a row can be superseded at most once. Many rows may carry
    -- a null here - those are the roots, one per transaction, and the
    -- partial index below is what limits them.
    constraint network_transaction_superseded_once unique (supersedes_id),
    constraint network_transaction_not_own_predecessor
        check (supersedes_id is null or supersedes_id <> id)
);

comment on table cashback.network_transaction is
    'IMMUTABLE (C-3). Exactly what a network reported, when, and for which query window. A status or amount change is a NEW row superseding the previous one; nothing here is ever edited, because a member''s money rests on it.';
comment on column cashback.network_transaction.status_raw is
    'The network''s own status vocabulary, verbatim. Kept beside the normalised status so a mapping bug is provable from the evidence rather than argued from memory (FR-032).';
comment on column cashback.network_transaction.status is
    'The normalised domain status (FR-033): pending to confirmed or declined, plus reversed from either.';
comment on column cashback.network_transaction.commission_minor is
    'The commission the network reported, in minor units, with an explicit currency (C-6). The member''s share is computed from THIS figure and the click-time rate, never from the published rate today.';
comment on column cashback.network_transaction.raw_payload is
    'The network''s payload verbatim (FR-032). Normalisation can be wrong; the payload is what was actually said.';
comment on column cashback.network_transaction.content_digest is
    'A database-computed fingerprint of the reported facts - click reference, both statuses, both amounts, currency and transaction time. It is what makes an unchanged re-report a no-op and a changed one a new row. Written by trigger: a caller-supplied value is discarded.';
comment on column cashback.network_transaction.supersedes_id is
    'The row this report replaces. Set on the NEW row, because the old one is immutable and cannot be marked. One root per transaction plus no forks means one chain, whose tip is the current row.';

create index network_transaction_click_ref_idx on cashback.network_transaction (click_ref)
    where click_ref is not null;
create index network_transaction_network_account_idx
    on cashback.network_transaction (network_account_id, retrieved_at desc);
create index network_transaction_transacted_at_idx on cashback.network_transaction (transacted_at);

-- Exactly one root per reported transaction. Together with the uniqueness
-- of supersedes_id above, this is what "exactly one current row" is derived
-- from - see the header comment.
create unique index network_transaction_one_root
    on cashback.network_transaction (network_id, external_id)
    where supersedes_id is null;

-- The digest is computed by the database from the reported facts, exactly
-- as source_item.content_hash is computed from the retrieved body. The
-- fingerprint can then never disagree with the evidence it fingerprints.
--
-- It deliberately does NOT hash raw_payload: networks include their own
-- response timestamps and pagination metadata in a payload, which would
-- make every re-report look like a change and every poll create a new row.
-- The facts that decide whether anything actually changed are hashed
-- instead, with a unit separator between fields so two different splits
-- cannot produce one digest.
create function cashback.network_transaction_guard() returns trigger
language plpgsql
as $$
declare
    predecessor record;
begin
    -- sha256() is a pg_catalog builtin, deliberately in preference to
    -- pgcrypto's digest(): this body is resolved at CALL time, and the
    -- extension does not live in the same schema on every deployment.
    new.content_digest := encode(sha256(convert_to(
        coalesce(new.click_ref, '') || chr(31) ||
        new.status_raw || chr(31) ||
        new.status || chr(31) ||
        new.sale_amount_minor::text || chr(31) ||
        new.commission_minor::text || chr(31) ||
        new.currency || chr(31) ||
        to_char(new.transacted_at at time zone 'UTC', 'YYYY-MM-DD HH24:MI:SS.US'),
        'UTF8')), 'hex');

    -- A superseding report must be about the same transaction at the same
    -- network. Without this, a chain could be attached to another
    -- transaction's row: that transaction would gain a second tip while
    -- this one lost its root, and "the current row" would stop being a
    -- well-defined thing. A composite foreign key cannot express it,
    -- because the referenced columns are not the referenced row's key.
    if new.supersedes_id is not null then
        select nt.network_id, nt.external_id
          into predecessor
          from cashback.network_transaction nt
         where nt.id = new.supersedes_id;
        -- No row means a nonexistent predecessor; fall through so the
        -- foreign key raises it with its natural SQLSTATE.
        if found and (predecessor.network_id is distinct from new.network_id
            or predecessor.external_id is distinct from new.external_id) then
            raise exception 'a superseding network_transaction must report the same transaction at the same network (C-3): % / % cannot supersede % / %',
                new.network_id, new.external_id, predecessor.network_id, predecessor.external_id;
        end if;
    end if;
    return new;
end;
$$;

comment on function cashback.network_transaction_guard() is
    'BEFORE INSERT guard on network_transaction: computes content_digest from the reported facts (the caller is not the authority on it), and keeps a superseding report inside the chain of the transaction it supersedes.';

create trigger network_transaction_guard
    before insert on cashback.network_transaction
    for each row execute function cashback.network_transaction_guard();

create trigger network_transaction_immutable
    before update or delete on cashback.network_transaction
    for each row execute function public.raise_immutable();
create trigger network_transaction_no_truncate
    before truncate on cashback.network_transaction
    for each statement execute function public.raise_immutable();
