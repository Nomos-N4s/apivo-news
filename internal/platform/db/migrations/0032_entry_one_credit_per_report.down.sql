-- Reverses 0032_entry_one_credit_per_report.

drop index cashback.entry_one_per_report;

alter table cashback.entry
    add constraint entry_one_per_report unique (network_transaction_id);
