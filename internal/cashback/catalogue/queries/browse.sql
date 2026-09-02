-- Browsing the catalogue: which merchants a reader in one place may see,
-- and what they are called (T102, US5 scenario 1, FR-010).
--
-- Place and language are INDEPENDENT axes (constitution VII), so they are
-- separate queries rather than one join: a reader in Munich reading Greek
-- and a reader in Munich reading German see the same merchants described
-- differently, and folding the two together would make that accidental.
--
-- "Available to a place" walks UP the place tree, which is the whole of
-- US5's first scenario: a member in Munich browses merchants relevant to
-- Munich AND to Germany. A retailer attached to the Germany place is
-- available in every town in it, and one attached to Munich is not
-- available in Hamburg. Walking up rather than down is the direction that
-- makes a national retailer one row instead of one per town.
--
-- A merchant with NO merchant_place row is available NOWHERE, deliberately.
-- The alternative reading - unscoped means everywhere - turns an import
-- that forgot to attach places into a retailer flooding every catalogue in
-- every country, which is a failure nobody sees until a member clicks
-- something they cannot buy from. Invisible-until-scoped is the state an
-- operator can find and fix. A genuinely national retailer attaches to the
-- country's own place, which is what the tree is for.

-- name: MerchantsForPlace :many
-- Every merchant a reader in this place may see, oldest slug first.
--
-- Only active merchants: merchant.status is whether we publish them at all,
-- independent of any single network route. A paused retailer keeps its
-- rows, its history and its offers, and stops appearing here.
--
-- The recursive term walks parent_id from the requested place to the root.
-- place_not_own_parent stops the one-step cycle the schema can express;
-- a longer cycle would be data nothing in this system creates, and
-- UNION (not UNION ALL) makes even that terminate rather than spin.
with recursive scope as (
    select p.id, p.parent_id
      from place p
     where p.id = sqlc.arg(place_id)
    union
    select up.id, up.parent_id
      from place up
      join scope s on s.parent_id = up.id
)
select
    m.id,
    m.slug,
    m.country,
    m.source_language_code,
    m.status
  from cashback.merchant m
 where m.status = 'active'
   and exists (
       select 1
         from cashback.merchant_place mp
         join scope s on s.id = mp.place_id
        where mp.merchant_id = m.id
   )
 order by m.slug;

-- name: CopyForMerchants :many
-- Every language each of these merchants is described in, so the caller can
-- resolve one per reader and label a fallback (T101).
--
-- ALL languages rather than the reader's, because the resolution rule needs
-- to see what is available to know whether what it picked is a fallback -
-- and a query that returned only the asked-for language would answer
-- "nothing" identically for a merchant with no copy at all and one whose
-- copy is simply in another language. Those are a bug and a fallback.
select
    mc.merchant_id,
    mc.language_code,
    mc.name,
    mc.summary,
    mc.terms
  from cashback.merchant_copy mc
 where mc.merchant_id = any (sqlc.arg(merchant_ids)::uuid[])
 order by mc.merchant_id, mc.language_code;
