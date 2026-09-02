-- The reads the hold rules make (T118, US7).
--
-- Each rule is one question about what already happened, asked at the
-- moment a credit is about to open: how many accounts share the click's
-- context, how many credits this member has had lately, how old the account
-- is. Counts and instants, never rows - a rule needs a number to compare,
-- not the evidence behind it.

-- name: AccountsSharingContext :one
-- How many distinct member accounts clicked with this context digest since
-- an instant. The digest is FR-022's privacy-minimised device digest (T066):
-- equal inputs give equal digests, which is the whole of what a self-dealing
-- rule needs - one device behind many accounts - and nothing here reads what
-- the digest was made from.
select count(distinct c.account_id)
  from cashback.click c
 where c.context_digest = sqlc.arg(context_digest)
   and c.clicked_at >= sqlc.arg(since);

-- name: MemberCreditsSince :one
-- How many credits have been opened for this member since an instant.
-- Credits only: a reversing entry is money going back, not a purchase, and
-- counting it would make a member the network clawed back from look busier.
select count(*)
  from cashback.entry e
 where e.account_id = sqlc.arg(account_id)
   and e.reversal_of_id is null
   and e.created_at >= sqlc.arg(since);

-- name: AccountCreatedAt :one
-- When the member's account was created. public.account is the shared
-- reference data both products read (ADR-0001), which is what makes the
-- read lawful across the schema boundary.
select a.created_at
  from public.account a
 where a.id = sqlc.arg(id);
