-- Reverses 0030_reconciliation_difference_verdict.
--
-- Dropping the verdict keeps who, when and why: a resolved row stays
-- resolved, it just no longer says which side was right.

alter table cashback.reconciliation_difference
    drop constraint reconciliation_difference_resolution_all_or_none;

alter table cashback.reconciliation_difference
    add constraint reconciliation_difference_resolution_all_or_none
        check (num_nonnulls(resolved_by, resolved_reason, resolved_at) in (0, 3));

alter table cashback.reconciliation_difference
    drop column resolution;
