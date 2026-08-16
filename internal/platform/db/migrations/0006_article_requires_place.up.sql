-- 0006_article_requires_place: an article must name at least one place.
--
-- The front page is scoped by place (FR-009): ListFrontPage shows an
-- article only where an article_place row joins it to a followed place.
-- An article with no such row is therefore invisible to every reader on
-- every front page - published, approved, attributed, and unreachable,
-- silently, behind a 200 and an empty list. That state served no purpose
-- and carried a real cost, so it becomes unrepresentable: the decision is
-- the database's, not a field somebody remembers to pass (I-1 spirit,
-- same reasoning as the named-approver rule).
--
-- The shape is an AFTER INSERT constraint trigger, DEFERRABLE INITIALLY
-- DEFERRED, not a NOT NULL column: places are many-to-many and the schema
-- already models them as article_place rows, so the rule is "at least one
-- row exists", which no column constraint can say. DEFERRED matters
-- twice. It lets the article insert and its article_place rows commit
-- together in one transaction - the approval writes both, in either
-- order. And because the check runs at COMMIT, a test that seeds an
-- article inside a rolled-back transaction is undisturbed: what never
-- commits is never checked.
--
-- The rule holds in both directions of time, not only at the article's
-- birth. Backward: AFTER INSERT judges no row that already exists, so a
-- validation block below refuses to apply this migration over a database
-- that already holds a placeless article - the migration certifies the
-- invariant, it does not invent the places. Forward: article_place rows
-- are insert-only in practice, but practice is not a guarantee, so a
-- second deferred trigger raises at COMMIT when deleting or re-homing an
-- article's place rows would leave it with none. Withdrawal is untouched
-- either way - a withdrawn article keeps its places as the record of
-- where it appeared - and article rows themselves cannot be deleted at
-- all (article_no_delete, 0001), so "an article left placeless" is the
-- only escape this trigger has to close.

-- Refuse to certify a world this migration did not create: an article
-- already placeless when 0006 arrives would keep its invisibility, now
-- with a certificate. In the style of 0002's reference checks, the count
-- is named and the operator assigns places explicitly before applying.
-- Pre-release the article table is empty in every environment and this
-- block never fires; it exists so the migration cannot gamble on that.
do $$
declare
    placeless bigint;
begin
    select count(*) into placeless
    from article a
    where not exists (
        select 1 from article_place ap where ap.article_id = a.id
    );
    if placeless > 0 then
        raise exception '% existing article(s) name no place (FR-009): assign each at least one article_place row before applying 0006 - this migration certifies the invariant, it does not invent the places', placeless;
    end if;
end;
$$;

create function article_requires_place() returns trigger
language plpgsql
as $$
begin
    if not exists (
        select 1 from article_place where article_id = new.id
    ) then
        raise exception 'article % names no place (FR-009): an article with no article_place row can appear on no front page, so it must be inserted in the same transaction as at least one', new.id;
    end if;
    return null;
end;
$$;

comment on function article_requires_place() is
    'COMMIT-time check (deferred constraint trigger) that an inserted article has at least one article_place row: the front page is scoped by place, so a placeless article would be unreachable by every reader (FR-009).';

create constraint trigger article_requires_place
    after insert on article
    deferrable initially deferred
    for each row execute function article_requires_place();

create function article_keeps_place() returns trigger
language plpgsql
as $$
begin
    if not exists (
        select 1 from article_place where article_id = old.article_id
    ) then
        raise exception 'article % would be left naming no place (FR-009): an article with no article_place row can appear on no front page - a transaction may rearrange an article''s places, never take the last one', old.article_id;
    end if;
    return null;
end;
$$;

comment on function article_keeps_place() is
    'COMMIT-time check (deferred constraint trigger) that deleting or re-homing article_place rows leaves no article placeless: the delete-side twin of article_requires_place(), so the FR-009 invariant holds for the article''s whole life, not just its birth.';

create constraint trigger article_keeps_place
    after delete or update on article_place
    deferrable initially deferred
    for each row execute function article_keeps_place();
