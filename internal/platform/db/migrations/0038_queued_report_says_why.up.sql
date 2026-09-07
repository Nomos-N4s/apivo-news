-- 0038_queued_report_says_why: an unattributed queue row says WHY it is
-- there, because the five causes that reach it have five different right
-- answers (spec 004, FR-098, T225).
--
-- 0013 gave this queue one discriminator, and said exactly what it was for:
--
--   'Which resolutions are legal, derived from immutable evidence rather
--    than stored... this column is what lets the operator surface tell them
--    apart without a kind column and without a migration.'
--        - 0013, on the derived `attributable`
--
-- That was true of the two causes that existed. It is not true of five.
-- Everything except "the network named no reference at all" now reads
-- attributable = false:
--
--   no_reference            the network reported no click reference at all
--   unknown_reference       a reference matching no click we ever issued
--   foreign_network         a reference matching a click ANOTHER network
--                           issued (0037, FR-096)
--   click_already_credited  the click it names already earned its one
--                           credit (0034, entry_click_id_idx)
--   foreign_currency        the report is in a currency its member cannot
--                           be paid in (0036, entry_currency_is_the_members)
--   route_cannot_attribute  the route carries no click reference by design
--                           (rule 11, merchant_network.can_attribute)
--
-- and the operator actions differ as far as it is possible to differ.
-- Attribute the first by hand. Dismiss the second. For foreign_network do
-- NOTHING: two networks reporting one purchase is the CORRECT outcome of a
-- two-network deployment, and an operator's instinct - attribute it by hand
-- - is exactly the second credit 0034 now refuses. A queue that cannot say
-- which is which teaches the wrong lesson at the only moment it matters.
--
-- STORED, not derived, which departs from 0013 and needs its reason. The
-- statement that queues a report has already decided: its own WHERE clause
-- IS the cause, and it now writes down what it decided. Deriving would
-- restate all five predicates in a sixth place - the second implementation
-- every statement in this schema warns about - and would let the answer
-- change under an operator between the page and the click, which is the
-- race the ops surface re-reads to prevent. `attributable` is untouched
-- beside it: why this row is here, and whether it may lawfully be
-- attributed, are different questions with different answers.
--
-- route_cannot_attribute is admitted with nothing writing it yet. The
-- contract names it, and its writer arrives when the importer learns
-- can_attribute from a network (T233/T241); admitting it now means that
-- lands without widening an enum a client has already been shown.
--
-- If this migration fails, the rows it names are queue rows whose cause
-- nothing derives - which should be unreachable - and they are an
-- operator's to look at before it is run again:
--
--   select u.id, u.network_transaction_id, nt.click_ref, nt.network_id
--     from cashback.unattributed_transaction u
--     join cashback.network_transaction nt on nt.id = u.network_transaction_id
--    where u.reason is null;

alter table cashback.unattributed_transaction
    add column reason text;

-- The backfill derives each existing row from the evidence, once, before
-- anything can change under it. The branches are ordered as the code
-- decides, and the order is load-bearing in one place: a report that named
-- no reference keeps no_reference even after an operator attributed it by
-- hand, because a report with no reference never grows one and the later
-- entry must not re-label what was seen.
update cashback.unattributed_transaction u
   set reason = case
       when nt.click_ref is null then 'no_reference'
       when not exists (
           select 1 from cashback.click c where c.click_ref = nt.click_ref
       ) then 'unknown_reference'
       when exists (
           select 1 from cashback.click c
            where c.click_ref = nt.click_ref
              and c.network_id <> nt.network_id
       ) then 'foreign_network'
       when exists (
           select 1 from cashback.click c
            join cashback.entry e on e.click_id = c.id and e.reversal_of_id is null
            where c.click_ref = nt.click_ref
              and c.network_id = nt.network_id
       ) then 'click_already_credited'
       when exists (
           select 1 from cashback.click c
            left join cashback.participation p on p.account_id = c.account_id
            where c.click_ref = nt.click_ref
              and c.network_id = nt.network_id
              and (p.account_id is null or p.default_currency <> nt.currency)
       ) then 'foreign_currency'
   end
  from cashback.network_transaction nt
 where nt.id = u.network_transaction_id;

alter table cashback.unattributed_transaction
    alter column reason set not null,
    add constraint unattributed_reason_known
        check (reason in ('no_reference', 'unknown_reference', 'foreign_network',
                          'click_already_credited', 'foreign_currency',
                          'route_cannot_attribute'));

comment on column cashback.unattributed_transaction.reason is
    'Why this report could not be credited, as the statement that queued it decided (FR-098). Frozen with the rest of the observation: it records what was true when the row was written, not what would be true if the question were asked again now.';

comment on constraint unattributed_reason_known on cashback.unattributed_transaction is
    'The six causes a report reaches this queue by. route_cannot_attribute is admitted before its writer exists (T233/T241) so that writer does not widen an enum clients have already seen.';

-- The reason joins the frozen half of the row. It is an observation, like
-- which report and when it was noticed - not a decision anyone may still
-- make. Re-labelling a queue row after the fact would rewrite what an
-- operator was actually shown, which is the whole thing 0013 and 0024 exist
-- to prevent.
create or replace function cashback.unattributed_transaction_guard() returns trigger
language plpgsql
as $$
begin
    if new.id is distinct from old.id
        or new.network_transaction_id is distinct from old.network_transaction_id
        or new.detected_at is distinct from old.detected_at
        or new.reason is distinct from old.reason then
        raise exception 'an unattributed transaction names which report went unattributed, when it was noticed and why (FR-034, FR-098): all three are frozen, only the resolution may be recorded';
    end if;
    -- Appended to, never erased. The all-or-none check still governs the
    -- shape of a resolution; this governs its direction.
    if old.resolved_at is not null and new.resolved_at is null then
        raise exception 'an unattributed transaction records who resolved it and why (FR-061): a resolution is appended to, never erased - correct a mistaken one by appending to domain_event';
    end if;
    return new;
end;
$$;

comment on function cashback.unattributed_transaction_guard() is
    'Freezes which report an unattributed queue row refers to, when it was detected and why it was queued (FR-098), and refuses to let a recorded resolution be erased (FR-061); only resolved_by, resolved_reason and resolved_at may be written, and only once.';
