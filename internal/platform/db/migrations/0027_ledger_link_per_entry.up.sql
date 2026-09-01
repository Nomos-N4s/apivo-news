-- 0027_ledger_link_per_entry: one transfer may carry several entries.
--
-- 0013 declared, in a single-column unique constraint:
--
--     constraint ledger_link_transfer_ref_unique unique (ledger_transfer_ref)
--
-- and its comment read "unique here so one transfer can never be claimed by
-- two transitions". That was true of every transfer the schema had at the
-- time: an entry opening, confirming or reversing gets a transfer of its own,
-- keyed on the entry, the cause and the destination state (D8).
--
-- A reservation is not one of those, and 0016 already says so. The provenance
-- view answers C-7 - which entries did this payout pay - with
--
--     join cashback.entry_transition et
--       on et.ledger_transfer_ref = wr.reserved_transfer_ref
--
-- against a request whose reserved_transfer_ref is a single column. That join
-- returns the whole payment only if EVERY entry a withdrawal reserved carries
-- that one reference, which means one transfer over several transitions -
-- exactly what the constraint above forbids. The two migrations disagreed,
-- and 0013's constraint is the one that has to move: D9 requires the
-- reservation to be one transfer (it is one movement of money), and C-7
-- requires the entries to be findable from it.
--
-- What the constraint was protecting is kept. "One posting per transition" is
-- ledger_link_pkey, on transition_id, and it is untouched. "A posting cannot
-- disagree with the transition it belongs to" is
-- ledger_link_matches_transition, the composite foreign key, and it is
-- untouched. What is left of the old constraint - that no two ENTRIES may
-- share a transfer - is what a reservation legitimately does, so it is
-- narrowed to the pair: one entry still cannot claim one transfer twice.

alter table cashback.ledger_link
    drop constraint ledger_link_transfer_ref_unique;

alter table cashback.ledger_link
    add constraint ledger_link_transfer_ref_per_entry_unique
        unique (ledger_transfer_ref, entry_id);

comment on column cashback.ledger_link.ledger_transfer_ref is
    'The ledger''s own transfer reference. Unique per ENTRY rather than outright: a withdrawal reserves several entries under one transfer (D9), and C-7 finds them by matching it (0016). One entry still cannot claim one transfer twice.';
