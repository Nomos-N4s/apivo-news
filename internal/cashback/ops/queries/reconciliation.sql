-- Importing a network's statement (T110, US6, C-3).
--
-- A statement is the counterparty's own account of what it paid for a
-- period. 0015 makes the row it lands in immutable, which is why everything
-- about it is decided BEFORE this insert runs: the Go side refuses a
-- statement it cannot read, because a row that cannot be corrected must not
-- be written wrong. This file owns only the writing, and the one question
-- the database alone can answer - whether this statement is already here.

-- name: LookUpNetworkAccount :one
-- The publisher account a statement is imported against.
--
-- Read before the insert rather than left to the foreign key, so an id that
-- names no account is answered as "no such account" instead of as a
-- constraint name - and so the account's network is known to the caller,
-- which is what the event announcing the import carries.
select id, network_id, active
  from cashback.network_account
 where id = sqlc.arg(id);

-- name: ImportStatement :one
-- Write the run, unless this exact statement for this account and period is
-- already here (reconciliation_run_statement_once, 0028).
--
-- ON CONFLICT DO NOTHING rather than a lookup first: two operators, or one
-- operator's retry, importing the same statement at the same moment would
-- both pass a lookup and one would then fail on the constraint. Letting the
-- constraint decide gives the second caller no row instead of an error, and
-- ImportedStatement then reads the run the first caller wrote.
--
-- No trigger fires on the conflict path: DO NOTHING is not an update, so the
-- immutability guard has nothing to refuse.
insert into cashback.reconciliation_run
    (network_account_id, statement_period_start, statement_period_end, imported_by, raw_statement)
values (sqlc.arg(network_account_id), sqlc.arg(statement_period_start), sqlc.arg(statement_period_end),
        sqlc.arg(imported_by), sqlc.arg(raw_statement))
on conflict on constraint reconciliation_run_statement_once do nothing
returning id, imported_by, imported_at, statement_digest;

-- name: ImportedStatement :one
-- The run an identical import already produced: same account, same period,
-- same statement. The digest is computed here the way the column computes
-- it, from the jsonb form, so a caller's formatting cannot make one
-- statement look like two.
select id, imported_by, imported_at, statement_digest
  from cashback.reconciliation_run
 where network_account_id = sqlc.arg(network_account_id)
   and statement_period_start = sqlc.arg(statement_period_start)
   and statement_period_end = sqlc.arg(statement_period_end)
   and statement_digest = md5(sqlc.arg(raw_statement)::jsonb::text);
