-- 0036_entry_currency_is_the_members: a member is only ever credited in the
-- currency their participation is denominated in - the one a withdrawal can
-- actually reserve and pay out (spec 004, FR-109, SC-027).
--
-- (specs/004-multi-network-linkwise/data-model.md describes this change
-- under 0036, after 0034 and 0035 took the click and the route rules.)
--
-- An entry's currency was whatever the network reported. Withdrawal is
-- deployment-wide single-currency: only entries in the payout threshold's
-- currency can be reserved, and a request in any other is refused. So an
-- entry in another currency was credited, confirmed, counted in the
-- member's wallet, and unwithdrawable - not refused, not queued, not
-- flagged. With one network and one market it could not arise; with two it
-- is one configuration away, and its symptom is a member looking at money
-- that does not move.
--
-- The deployment's payout currency is configuration and cannot be a check.
-- The MEMBER's currency is a column - participation.default_currency,
-- recorded when they accepted the terms - and an entry already belongs to
-- an account, so the rule that matters is a key: (account_id, currency)
-- on the entry references (account_id, default_currency) on the
-- participation. Two things follow, both wanted. An entry in a foreign
-- currency is unrepresentable: the crediting path recognises the refusal
-- by name and queues the report for an operator (FR-109). And a member's
-- currency cannot be restated while they hold entries in the old one:
-- their money is denominated in what they were credited in.
--
-- The key also makes crediting require an opt-in, which is why the
-- click-out gate (FR-110, 0034's contemporary in code) landed first: a
-- member who cannot click cannot be credited, so the refusal never lands
-- on somebody who has already bought something.
--
-- If this migration fails, the rows it names are entries credited outside
-- their member's currency, or to a member who never opted in - both states
-- this rule exists to make impossible - and they are an operator's to
-- resolve before it is run again:
--
--   select e.id, e.account_id, e.currency, p.default_currency
--     from cashback.entry e
--     left join cashback.participation p on p.account_id = e.account_id
--    where p.account_id is null or p.default_currency <> e.currency;
--
-- network_account.reports_currency is FR-108's half: the refusal one step
-- earlier, at connection time, where a person can act on it. Nullable on
-- purpose - an account connected before its adapter has a recording has
-- nothing honest to declare, and a default would be a guess about money.
-- connect-network refuses a DECLARED currency this deployment cannot pay
-- out; a null is reported, not refused.

alter table cashback.participation
    add constraint participation_account_currency_unique
    unique (account_id, default_currency);

comment on constraint participation_account_currency_unique
    on cashback.participation is
    'The key entry_currency_is_the_members joins on. Redundant against the primary key on account_id alone; it exists so the currency rule can be a foreign key rather than a trigger.';

alter table cashback.entry
    add constraint entry_currency_is_the_members
    foreign key (account_id, currency)
    references cashback.participation (account_id, default_currency);

comment on constraint entry_currency_is_the_members on cashback.entry is
    'A member is only ever credited in the currency their participation is denominated in - the one a withdrawal can actually reserve and pay out (FR-109). Without this, a network reporting in another currency produces a balance the member can see and can never withdraw, and nothing anywhere says so. It also refuses restating a participating member''s default_currency while an entry references the old one (SC-027).';

alter table cashback.network_account
    add column reports_currency char(3)
        constraint network_account_reports_currency_iso4217_format
            check (reports_currency is null or reports_currency ~ '^[A-Z]{3}$');

comment on column cashback.network_account.reports_currency is
    'The currency this publisher account''s network reports commission in, as declared when it was connected (FR-108). Null means nobody has established it yet - which is itself the state that produces an unwithdrawable balance, so an operator listing shows it as such rather than as blank.';
