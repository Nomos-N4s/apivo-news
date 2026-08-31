-- name: MemberEntries :many
-- One page of a member's own earnings, newest first (T079, US3).
--
-- Every entry, including the reversals: a reversing entry is a row of its
-- own citing the one it undoes (SC-010), so listing the member's entries
-- shows both halves of a clawback without either being hidden. Nothing here
-- filters reversals out, and nothing should - "the money came back" is the
-- fact a member most needs to see, and a list that quietly dropped one side
-- of the pair would leave a balance nobody could reconcile against it.
--
-- The merchant comes through the CLICK, because that is the only path from
-- an earning to the retailer it was earned at: a network's report names the
-- network and its own transaction, never a merchant this database knows.
-- Every join on that path is a LEFT join for the same reason: an entry an
-- operator attributed by hand has no click (FR-034), and it is still the
-- member's money and still belongs in their wallet.
--
-- The name is looked up twice - once in the language asked for, once in the
-- merchant's own - and both are returned. Which one a client shows is its
-- decision; what it must not do is show the fallback as though it were the
-- language the member reads (US5 scenario 2), and it cannot tell without
-- being told which one it got.
--
-- Keyset pagination on (created_at, id), never an offset. Entries are
-- inserted while a member pages, and an offset would show them a row twice
-- or not at all; the id breaks ties, because created_at is a clock and two
-- entries can share one.
select
    entry.id,
    entry.state,
    entry.amount_minor,
    entry.currency,
    entry.created_at,
    entry.reversal_of_id,
    entry.hold_rule,
    report.transacted_at,
    report.sale_amount_minor,
    report.currency as sale_currency,
    asked.name as name_in_language_asked,
    fallback.name as name_in_merchants_language,
    merchant.source_language_code,
    opening.reason
  from cashback.entry entry
  join cashback.network_transaction report
    on report.id = entry.network_transaction_id
  left join cashback.click click on click.id = entry.click_id
  left join cashback.offer offer on offer.id = click.offer_id
  left join cashback.merchant_network route on route.id = offer.merchant_network_id
  left join cashback.merchant merchant on merchant.id = route.merchant_id
  left join cashback.merchant_copy asked
    on asked.merchant_id = merchant.id
   and asked.language_code = sqlc.arg(language)::text
  left join cashback.merchant_copy fallback
    on fallback.merchant_id = merchant.id
   and fallback.language_code = merchant.source_language_code
  -- The transition that opened the entry carries why it exists, which for a
  -- reversal is why the money came back. Read from the opening rather than
  -- the latest, because a later release or confirmation carries a reason
  -- about itself and would overwrite the one the member is owed.
  left join lateral (
      select transition.reason
        from cashback.entry_transition transition
       where transition.entry_id = entry.id
         and transition.from_state is null
       order by transition.occurred_at
       limit 1
  ) opening on true
 where entry.account_id = sqlc.arg(account_id)
   and (sqlc.narg(state)::text is null or entry.state = sqlc.narg(state)::text)
   and (sqlc.narg(cursor_at)::timestamptz is null
        or (entry.created_at, entry.id) < (sqlc.narg(cursor_at)::timestamptz, sqlc.narg(cursor_id)::uuid))
 order by entry.created_at desc, entry.id desc
 limit sqlc.arg(page_size);
