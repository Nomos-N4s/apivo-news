-- 0031_accounting_export_indexes: the two journals accounting reads by
-- window, indexed by when.
--
-- FR-062 exports the ledger journal - every entry transition, each pinned
-- to its ledger transfer - and the reconciliation journal - every
-- difference, with its decision - for a period. Both are read by the
-- instant the row was written: entry_transition by occurred_at,
-- reconciliation_difference by detected_at. Neither had an index on that
-- alone. entry_transition is indexed by (entry_id, occurred_at), which
-- serves one entry's history and nothing else; reconciliation_difference
-- carries a partial index on detected_at for OPEN rows only, which is the
-- operator queue and excludes exactly the decided rows an accountant wants.
--
-- (when, id) rather than when alone, because the export pages on the pair:
-- two rows written in the same microsecond are ordered by id, and an index
-- that stops at the timestamp would leave that tie-break to a sort.

create index entry_transition_occurred_at_idx
    on cashback.entry_transition (occurred_at, id);

create index reconciliation_difference_detected_at_idx
    on cashback.reconciliation_difference (detected_at, id);
