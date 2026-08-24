-- 0017_participation: the member's opt-in.
--
-- ADR-0004 names four records where a tenant boundary would fall - member
-- participation, merchant availability, cashback entry, payout - and this
-- is the last of the four to arrive. The other three carry brand_id from
-- the migration that creates them (0011, 0013, 0014), and so does this
-- one: a column added by a later migration does not exist at the points in
-- the history before it, which is a trap for anything written against the
-- finished schema and run against a partial one.
--
-- There is deliberately no default. A brand literal in a migration is what
-- the constitution's rebrandability rule forbids, and a row whose brand
-- nobody stated is a row nobody can scope later.

create table cashback.participation (
    -- One participation per account (FR-001): the account IS the key, so a
    -- member cannot hold two opt-ins with different terms.
    account_id uuid primary key references public.account (id),
    brand_id text not null
        constraint participation_brand_id_not_blank check (btrim(brand_id) <> ''),
    opted_in_at timestamptz not null default now(),
    terms_version text not null
        constraint participation_terms_version_not_blank check (btrim(terms_version) <> ''),
    status text not null default 'active'
        constraint participation_status_known check (status in ('active', 'left')),
    left_at timestamptz,
    default_currency char(3) not null
        constraint participation_default_currency_iso4217_format
            check (default_currency ~ '^[A-Z]{3}$'),
    -- Left iff there is a leaving date. "Left, but we do not know when" and
    -- "active, but with a leaving date" are both unrepresentable.
    constraint participation_left_has_timestamp
        check ((status = 'left') = (left_at is not null)),
    constraint participation_left_after_opt_in
        check (left_at is null or left_at >= opted_in_at)
);

comment on table cashback.participation is
    'A member''s opt-in to cashback (FR-001). Leaving closes the row; it never deletes the financial record built on it (FR-003), because entries, payouts and evidence outlive participation by law and by accounting.';
comment on column cashback.participation.terms_version is
    'Which version of the terms this member accepted (FR-002). Never blank: "they agreed to something" is not a record anyone can rely on later.';
comment on column cashback.participation.default_currency is
    'The currency this member''s wallet is denominated in, as an explicit ISO-4217 code (C-6). Configuration supplies the value; the schema carries no default, because a currency default is a brand decision (ADR-0004).';
comment on column cashback.participation.status is
    'active or left. Leaving is a status change and a timestamp, never a deletion.';

create index participation_brand_status_idx on cashback.participation (brand_id, status);

-- The row is the evidence that this member accepted these terms on this
-- date (FR-002). Evidence that can be rewritten is not evidence, and
-- FR-003's "leaving never deletes" is not a rule the schema can leave to
-- whoever writes the delete. Same treatment as consent history in 0001:
-- no deletes, identity frozen, and only the lifecycle transitions move.
--
-- Re-joining is the one case where the accepted terms legitimately change,
-- and it is a transition rather than a quiet edit: a member may leave and
-- come back, and coming back means accepting the terms in force then. So
-- opted_in_at, terms_version and default_currency are frozen EXCEPT on the
-- left -> active transition, which re-states all of them together. Every
-- acceptance is also appended to domain_event, which is where the full
-- history of who accepted what and when survives a re-join.
create function cashback.participation_guard() returns trigger
language plpgsql
as $$
begin
    if tg_op = 'DELETE' then
        raise exception 'participation is closed, never deleted (FR-003): leaving is a status and a date, and the financial record built on it outlives the preference';
    end if;
    if new.account_id is distinct from old.account_id
        or new.brand_id is distinct from old.brand_id then
        raise exception 'participation identity is frozen: whose opt-in it is, and to which brand, cannot be reassigned';
    end if;
    -- Re-joining re-states the acceptance; nothing else may touch it.
    if not (old.status = 'left' and new.status = 'active') then
        if new.opted_in_at is distinct from old.opted_in_at
            or new.terms_version is distinct from old.terms_version
            or new.default_currency is distinct from old.default_currency then
            raise exception 'the accepted terms and the date they were accepted are frozen (FR-002): they may only be re-stated by rejoining after leaving';
        end if;
    end if;
    return new;
end;
$$;

comment on function cashback.participation_guard() is
    'Protects the opt-in record: no deletes (FR-003), identity frozen, and the accepted terms re-statable only by the left -> active rejoin transition (FR-002).';

create trigger participation_guard
    before update or delete on cashback.participation
    for each row execute function cashback.participation_guard();
create trigger participation_no_truncate
    before truncate on cashback.participation
    for each statement execute function public.raise_immutable();
