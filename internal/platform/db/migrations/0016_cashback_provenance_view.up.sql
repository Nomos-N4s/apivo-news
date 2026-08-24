-- 0016_cashback_provenance_view: the two questions an auditor asks.
--
--   C-7  For any member payout, ONE query returns the full chain - payout,
--        approver, ledger postings, cashback entries, network transaction
--        evidence, click, and the offer rate at click time - in under five
--        minutes.
--   C-1  A member balance is never a settable number. It exists only as
--        the sum of immutable ledger postings, and every posting belongs
--        to a transfer whose postings sum to zero per currency.
--
-- C-7 is cashback's article_provenance: one view, one query, no assembly
-- by the caller. The seam that makes it a single join is D9's reservation
-- transfer - the withdrawal reserves the entries it will pay, each
-- reserved entry records that same transfer reference on its transition,
-- and so a payout can reach its entries without a foreign key that would
-- have to be kept in step by hand.
--
-- C-1 is the one invariant that lives outside this schema (ADR-0002,
-- named as the Principle VIII exception). The constitution's mitigation is
-- that the check is "a real SQL query over real rows" and that it fails
-- loudly. This migration provides that query. It is deliberately written
-- so the schema also works where the ledger is not co-located: with no
-- ledger schema present the view is empty and the check is vacuously true,
-- which is honest - there are no postings to disagree about - rather than
-- an error that would have to be suppressed in CI and would then be
-- suppressed everywhere.

------------------------------------------------------------------------------
-- C-7: payout -> approver -> postings -> entries -> evidence -> click.
------------------------------------------------------------------------------

create view cashback.provenance as
select
    p.id                        as payout_id,
    p.state                     as payout_state,
    p.rail,
    p.rail_reference,
    p.idempotency_key,
    p.amount_minor              as payout_amount_minor,
    p.currency                  as payout_currency,
    p.submitted_at,
    p.settled_at,
    approver.id                 as approver_id,
    approver.display_name       as approver_name,
    approver.email              as approver_email,
    wr.id                       as request_id,
    wr.account_id               as member_id,
    wr.requested_at,
    wr.reserved_transfer_ref,
    wr.decided_by,
    wr.decided_at,
    et.id                       as transition_id,
    et.from_state               as entry_from_state,
    et.to_state                 as entry_to_state,
    et.ledger_transfer_ref,
    et.occurred_at              as transition_at,
    ll.posted_at                as ledger_posted_at,
    e.id                        as entry_id,
    e.state                     as entry_state,
    e.amount_minor              as entry_amount_minor,
    e.currency                  as entry_currency,
    nt.id                       as network_transaction_id,
    nt.network_id,
    nt.external_id,
    nt.status                   as network_status,
    nt.status_raw               as network_status_raw,
    nt.commission_minor,
    nt.currency                 as commission_currency,
    nt.transacted_at,
    nt.retrieved_at             as evidence_retrieved_at,
    nt.content_digest,
    c.id                        as click_id,
    c.click_ref,
    c.clicked_at,
    -- The rate that GOVERNS this credit: the band as published at click
    -- time (FR-013), read from the immutable click, never from the offer
    -- row as it stands today.
    c.rate_snapshot,
    c.member_share_bps_snapshot,
    o.id                        as offer_id,
    o.rate_kind,
    -- The published rate NOW, carried only so an auditor can see whether
    -- it moved since the click. It is not what the credit was computed
    -- from; rate_snapshot above is.
    o.rate_bps                  as offer_rate_bps_current,
    mn.id                       as merchant_network_id,
    -- Which network's route the click was issued through. It follows from
    -- the rate band rather than being recorded beside it, so it cannot
    -- disagree with the band the credit was computed from.
    mn.network_id               as offer_network_id,
    mn.external_merchant_id,
    m.id                        as merchant_id,
    m.slug                      as merchant_slug
from cashback.payout p
join public.account approver on approver.id = p.approved_by
join cashback.withdrawal_request wr on wr.id = p.request_id
-- D9's reservation transfer is the seam: the entries this payout pays are
-- exactly the entries whose reservation transition carries that reference.
join cashback.entry_transition et on et.ledger_transfer_ref = wr.reserved_transfer_ref
join cashback.entry e on e.id = et.entry_id
left join cashback.ledger_link ll on ll.transition_id = et.id
join cashback.network_transaction nt on nt.id = e.network_transaction_id
left join cashback.click c on c.id = e.click_id
left join cashback.offer o on o.id = c.offer_id
left join cashback.merchant_network mn on mn.id = o.merchant_network_id
left join cashback.merchant m on m.id = mn.merchant_id;

comment on view cashback.provenance is
    'C-7: for any payout - approver, request, reserved entries, ledger postings, network evidence, click and the rate that governed the credit - in a single query. The click that earned it is left-joined because an operator-attributed entry legitimately has none; the network evidence is not, because C-2 makes it mandatory.';

------------------------------------------------------------------------------
-- C-1: the zero-sum check over the ledger's own rows (ADR-0002).
------------------------------------------------------------------------------


-- The ledger owns balances (ADR-0002), and the constitution's mitigation
-- for that exception is that the zero-sum check is "a real SQL query over
-- real rows" that fails loudly. This section is that query, written to
-- survive every deployment shape ADR-0002 allows:
--
--   * ledger co-located in this database (spike S1 passes) - it reads the
--     ledger's own balance rows;
--   * ledger in its own database (the S1 fallback) - the relation is
--     absent, the view is empty, and the same assertion becomes a periodic
--     reconciliation job against the other database;
--   * no ledger at all (CI without the sidecar, LEDGER_DRIVER=memory) -
--     empty, and "every row sums to zero" is vacuously and honestly true,
--     because there are no postings to disagree about.
--
-- Summing balances per currency IS summing postings per currency: every
-- balance is the sum of its own postings, so the two totals are the same
-- number reached from either end. The currency list is read from the
-- LEDGER, not from the cashback tables, so a house account holding a
-- currency no member entry uses is still checked.
--
-- If the ledger schema is present but does not carry the columns these
-- read, the dynamic query raises. That is deliberate: a zero-sum check
-- that cannot see the postings must fail loudly, never quietly return
-- nothing.

create function cashback.ledger_balance_relation() returns text
language plpgsql
stable
as $$
declare
    balance_relation text;
begin
    -- Two questions, and the difference between them is the whole point.
    -- "Is the ledger installed here at all" and "can I read its postings"
    -- have different right answers, and an earlier version of this
    -- function conflated them: it returned null for both, so a ledger that
    -- was present but unreadable produced an empty view and a C-1 check
    -- that passed while seeing nothing. The comment two paragraphs up
    -- promised the opposite. That is the failure this whole invariant
    -- exists to prevent, committed by the check itself.
    if not exists (select 1 from pg_namespace where nspname = 'blnk') then
        -- No ledger schema: not co-located. Vacuously true is honest here,
        -- because there are no postings in this database to disagree about.
        return null;
    end if;

    balance_relation := to_regclass('blnk.balances')::text;
    if balance_relation is null then
        raise exception 'the ledger schema is present but blnk.balances is not readable: the C-1 zero-sum check cannot see the postings it exists to sum, and a check that cannot see them must fail rather than report zero rows';
    end if;
    return balance_relation;
end;
$$;

comment on function cashback.ledger_balance_relation() is
    'The ledger''s balance relation. Null only when the ledger is not co-located in this database at all; RAISES when the ledger schema is present but its balances cannot be read, because a zero-sum check that cannot see the postings must fail loudly rather than pass on nothing (C-1, ADR-0002).';

create function cashback.ledger_currencies() returns text[]
language plpgsql
stable
as $$
declare
    balance_relation text := cashback.ledger_balance_relation();
    currencies text[];
begin
    if balance_relation is null then
        return array[]::text[];
    end if;
    execute format('select array_agg(distinct b.currency::text) from %s b', balance_relation)
       into currencies;
    return coalesce(currencies, array[]::text[]);
end;
$$;

comment on function cashback.ledger_currencies() is
    'Every currency the ledger holds a balance in. Read from the ledger rather than from the cashback tables, so a house account in a currency no member entry uses is still covered by the zero-sum check.';

create function cashback.ledger_net_minor(p_currency text) returns bigint
language plpgsql
stable
as $$
declare
    balance_relation text := cashback.ledger_balance_relation();
    net bigint;
begin
    if balance_relation is null then
        return null;
    end if;
    execute format('select sum(b.balance)::bigint from %s b where b.currency::text = $1', balance_relation)
       into net
      using p_currency;
    return net;
end;
$$;

comment on function cashback.ledger_net_minor(text) is
    'The net of every ledger balance in one currency, in minor units. Zero is the only correct answer (C-1): anything else means money was created or destroyed inside the ledger.';

-- The casts are load-bearing for the generated code, not decoration:
-- unnest() over an array is polymorphic, and without them sqlc cannot infer
-- the column type and emits `Currency interface{}` - a money-adjacent
-- column that every caller has to type-assert.
create view cashback.ledger_zero_sum as
select
    ledger.currency::text as currency,
    cashback.ledger_net_minor(ledger.currency::text) as net_minor
from unnest(cashback.ledger_currencies()) as ledger(currency);

comment on view cashback.ledger_zero_sum is
    'C-1: every row must have net_minor = 0. A non-zero row means money was created or destroyed inside the ledger and is treated as an incident, not as a metric (SC-003). No rows means the ledger is not co-located in this database - the same assertion then runs as a periodic job against wherever it lives.';
