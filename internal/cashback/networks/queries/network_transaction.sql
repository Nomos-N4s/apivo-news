-- Network transaction queries (T052). The write path for the evidence a
-- member's money rests on, and the reads that path needs to answer for
-- itself.
--
-- Two rules from 0012 shape every statement in this file, and neither is
-- this file's to relax:
--
--   * C-3, immutability. There is no UPDATE and no DELETE here, and there
--     never will be: the table's triggers raise on both. A status change is
--     a new row that supersedes the old one, which is why the chain is
--     built forwards (the NEW row names its predecessor) rather than by
--     marking the old one superseded.
--   * content_digest is the DATABASE's. A BEFORE INSERT trigger computes it
--     from the reported facts and discards whatever the caller supplied, so
--     the insert below does not name the column at all. Naming it with a
--     placeholder would compile, pass, and quietly say that the caller is
--     an authority on the fingerprint of its own evidence - which is the
--     one thing this design refuses. NOT NULL is satisfied because
--     constraints are checked after BEFORE-row triggers have run.
--
-- The normalised columns and the verbatim payload are ONE ROW, so writing
-- them together needs no transaction to be atomic - the schema makes a
-- normalised record with no evidence behind it unrepresentable rather than
-- merely unlikely (FR-032). This file's job is not to undo that: nothing
-- here writes the two separately, and nothing could, because the second
-- write would be an UPDATE that the table refuses.

-- name: InsertNetworkTransaction :one
-- One report, exactly as a network stated it, with the retrieval facts the
-- poller adds: when the read was made and for which window. Those three are
-- Apivo's knowledge rather than the network's, which is why the Network port
-- does not carry them and this parameter list does.
--
-- supersedes_id is null for a first report and names the predecessor for a
-- superseding one (T054). It is a parameter from the start because the
-- column is part of what one report IS: a writer that could not express the
-- chain would have to be replaced rather than extended, and the guard
-- trigger already refuses a link that crosses to another transaction.
--
-- What comes back is what the DATABASE decided: the row's id, the digest it
-- computed, and the retrieval instant it stored. A caller that echoed its
-- own inputs back instead would report success for a row the database had
-- rewritten under it.
insert into cashback.network_transaction (
    network_id,
    network_account_id,
    external_id,
    click_ref,
    status_raw,
    status,
    sale_amount_minor,
    commission_minor,
    currency,
    transacted_at,
    retrieved_at,
    query_window_start,
    query_window_end,
    raw_payload,
    supersedes_id
) values (
    sqlc.arg(network_id),
    sqlc.arg(network_account_id),
    sqlc.arg(external_id),
    sqlc.narg(click_ref),
    sqlc.arg(status_raw),
    sqlc.arg(status),
    sqlc.arg(sale_amount_minor),
    sqlc.arg(commission_minor),
    sqlc.arg(currency),
    sqlc.arg(transacted_at),
    sqlc.arg(retrieved_at),
    sqlc.arg(query_window_start),
    sqlc.arg(query_window_end),
    sqlc.arg(raw_payload),
    sqlc.narg(supersedes_id)
)
returning id, content_digest, retrieved_at;

-- name: InsertNetworkTransactionIfNew :one
-- The same insert, with the one conflict that is not an error swallowed
-- (T053, US2 scenario 3). No rows back means the network re-reported a
-- transaction whose facts have not changed, and the row for it is already
-- stored.
--
-- ON CONFLICT rather than catching the violation in Go, and that is not a
-- style choice. A poller persists a whole window and advances its durable
-- cursor only once the window is written (FR-031), so every report of one
-- window shares a transaction - and in Postgres a failed statement aborts
-- the transaction it ran in. A raw unique violation on the third of forty
-- reports would take the other thirty-seven down with it and leave the
-- cursor where it was, so the window would be re-read forever.
--
-- The constraint is NAMED, and naming it is the whole correctness of this
-- query. An unchanged re-report violates two rules at once - the report is
-- already stored (unique_report) and it would be a second root for a
-- transaction that has one (one_root) - and only the first of those is
-- harmless. A bare `on conflict do nothing` would swallow both, so a report
-- whose status HAD changed, written as a root by mistake, would vanish
-- silently: the member's confirmed transaction would stay pending forever
-- with nothing logged. Targeting unique_report leaves one_root raising,
-- which is what says "this should have superseded something" (T054).
--
-- Verified against the server rather than reasoned about: with both rules
-- violated, Postgres takes this ON CONFLICT path and the transaction stays
-- usable; with only one_root violated, it still raises.
insert into cashback.network_transaction (
    network_id,
    network_account_id,
    external_id,
    click_ref,
    status_raw,
    status,
    sale_amount_minor,
    commission_minor,
    currency,
    transacted_at,
    retrieved_at,
    query_window_start,
    query_window_end,
    raw_payload,
    supersedes_id
) values (
    sqlc.arg(network_id),
    sqlc.arg(network_account_id),
    sqlc.arg(external_id),
    sqlc.narg(click_ref),
    sqlc.arg(status_raw),
    sqlc.arg(status),
    sqlc.arg(sale_amount_minor),
    sqlc.arg(commission_minor),
    sqlc.arg(currency),
    sqlc.arg(transacted_at),
    sqlc.arg(retrieved_at),
    sqlc.arg(query_window_start),
    sqlc.arg(query_window_end),
    sqlc.arg(raw_payload),
    sqlc.narg(supersedes_id)
)
on conflict on constraint network_transaction_unique_report do nothing
returning id, content_digest, retrieved_at;

-- name: GetCurrentNetworkTransaction :one
-- The transaction's current row: what the network last said about it, and
-- what a new report is judged against (T054).
--
-- "Current" is DERIVED rather than stamped, because the table is immutable
-- (C-3): a superseding report cannot mark its predecessor superseded, since
-- that would be an UPDATE the table refuses. 0012 builds the chain the other
-- way round - the new row names the old one - and then constrains it so that
-- exactly one row can be the tip: one root per transaction (a partial unique
-- index) and no forks (supersedes_id is unique). One root plus one path
-- means one tip, and the tip is the row nothing supersedes.
--
-- Which is what this asks. The NOT EXISTS is a single index probe rather
-- than a scan, because supersedes_id carries a unique constraint of its own,
-- so the question "is there a row superseding this one" has at most one
-- answer to find.
--
-- No rows means the network has never reported this transaction, which is a
-- first report rather than a failure.
select nt.*
from cashback.network_transaction nt
where nt.network_id = sqlc.arg(network_id)
  and nt.external_id = sqlc.arg(external_id)
  and not exists (
      select 1
      from cashback.network_transaction superseding
      where superseding.supersedes_id = nt.id
  );

-- name: GetNetworkTransaction :one
-- One stored report, whole. Every column, because this is evidence: a read
-- that returned a convenient subset would be the place a later question
-- about what was actually reported goes unanswerable.
select *
from cashback.network_transaction
where id = sqlc.arg(id);
