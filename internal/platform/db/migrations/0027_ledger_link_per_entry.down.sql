-- Reverses 0027_ledger_link_per_entry.
--
-- Reinstating the single-column constraint fails on any reservation that has
-- carried more than one entry, and that is correct rather than unfortunate:
-- those rows are what the constraint forbids, and dropping them to make the
-- down migration succeed would delete evidence a payout's provenance is read
-- from. A deployment that has to go back this far removes the reservations
-- first, deliberately.

alter table cashback.ledger_link
    drop constraint ledger_link_transfer_ref_per_entry_unique;

alter table cashback.ledger_link
    add constraint ledger_link_transfer_ref_unique unique (ledger_transfer_ref);

comment on column cashback.ledger_link.ledger_transfer_ref is
    'The ledger''s own transfer reference, unique here so one transfer can never be claimed by two transitions.';
