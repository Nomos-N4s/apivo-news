-- Reverses 0029_reconciliation_difference_identity.
--
-- Dropping the column loses which statement line a paid_not_reported
-- difference was about; the amount, the currency and the run stay. Any
-- resolution recorded against such a row stays with it.

drop index cashback.reconciliation_difference_one_per_line;
drop index cashback.reconciliation_difference_one_per_report;

alter table cashback.reconciliation_difference
    drop constraint reconciliation_difference_shape_matches_kind;

alter table cashback.reconciliation_difference
    add constraint reconciliation_difference_shape_matches_kind check (
        case kind
            when 'amount_mismatch' then network_transaction_id is not null
                and expected_minor is not null
                and actual_minor is not null
                and expected_minor <> actual_minor
            when 'reported_not_paid' then network_transaction_id is not null
                and expected_minor is not null
                and actual_minor is null
            when 'paid_not_reported' then network_transaction_id is null
                and expected_minor is null
                and actual_minor is not null
            else false
        end
    );

alter table cashback.reconciliation_difference
    drop column statement_transaction_id;
