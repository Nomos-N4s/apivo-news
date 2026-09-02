-- Reverses 0031_accounting_export_indexes.

drop index cashback.reconciliation_difference_detected_at_idx;
drop index cashback.entry_transition_occurred_at_idx;
