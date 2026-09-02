-- 0030_reconciliation_difference_verdict: a resolution says what was
-- decided, not only who decided and why.
--
-- 0015 records a resolution as who, when and why. The why is prose, and
-- prose is for a human reading one row; the queue (FR-060) and the
-- accounting export (FR-062) need to know, per row, what was decided.
--
-- Two verdicts, and both mean the same thing to the confirmation gate: this
-- disagreement no longer stands between the money and the member (FR-043).
-- explained: another fact accounts for it - a later statement paid it, the
-- network has since restated the report and the reversal followed, two
-- lines were one payment - and nothing is owed either way. absorbed: the
-- delta is the business's to bear or to keep, and the member's figure stands
-- as reported. What is NOT a verdict is "the network owes us and we are
-- chasing it": an open difference IS the chase, and it keeps the gate shut
-- until the money arrives or the network restates the report. Resolving it
-- early would confirm a member's balance out of money never received, which
-- is the one thing US6 exists to prevent.
--
-- The all-or-none rule grows to four columns: a verdict without a person, or
-- a person without a verdict, is a queue item that looks handled and is not.
-- Safe to tighten: nothing has resolved a difference before this migration
-- (resolution is what follows it), so no row exists to fail it.

alter table cashback.reconciliation_difference
    add column resolution text
        constraint reconciliation_difference_resolution_known
            check (resolution is null or resolution in ('explained', 'absorbed'));

comment on column cashback.reconciliation_difference.resolution is
    'What the operator decided (US6). explained: another fact accounts for the disagreement and nothing is owed either way. absorbed: the delta is the business''s to bear or to keep, and the member''s figure stands as reported. Either lifts the difference from the confirmation gate; a difference the network still owes on stays open. Set with resolved_by, resolved_reason and resolved_at - all four or none.';

alter table cashback.reconciliation_difference
    drop constraint reconciliation_difference_resolution_all_or_none;

alter table cashback.reconciliation_difference
    add constraint reconciliation_difference_resolution_all_or_none
        check (num_nonnulls(resolved_by, resolved_reason, resolved_at, resolution) in (0, 4));
