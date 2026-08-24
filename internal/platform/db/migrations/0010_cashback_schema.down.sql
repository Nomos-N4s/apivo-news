-- Reverses 0010_cashback_schema. Destroys the cashback schema and every
-- object 0011-0017 put in it: never run against an environment holding real
-- money records.

-- The grants go before the role does. Both the revokes and the DROP ROLE
-- below are written to tolerate the role already being gone: it is
-- cluster-wide, so another database's rollback can have removed it between
-- this statement and the last time anyone looked. Catching the error rather
-- than checking first is deliberate - a check can be overtaken, and a
-- rollback that fails because there was nothing left to revoke is worse
-- than useless.
--
-- The default privileges must be un-declared in the same shape they were
-- declared, or the entry outlives the schema in pg_default_acl.
do $$
begin
    execute 'alter default privileges in schema cashback revoke select, insert, update, delete on tables from cashback_domain';
    execute 'revoke select, insert on public.domain_event from cashback_domain';
    execute 'revoke select on public.language from cashback_domain';
    execute 'revoke select on public.place from cashback_domain';
    execute 'revoke select on public.account from cashback_domain';
    execute 'revoke usage on schema public from cashback_domain';
exception
    when undefined_object then
        raise notice 'role cashback_domain is no longer present in this cluster: its grants went with it';
end;
$$;

drop schema cashback cascade;

-- The role is cluster-wide: drop it only when nothing anywhere still
-- depends on it. Another database in the same cluster may hold grants this
-- migration cannot see, and DROP ROLE fails loudly in that case rather than
-- silently stranding them - so the failure is caught and the role is left
-- in place, which is the safe outcome.
do $$
begin
    drop role cashback_domain;
exception
    when undefined_object then
        raise notice 'role cashback_domain was already dropped elsewhere in this cluster';
    when dependent_objects_still_exist or insufficient_privilege then
        raise notice 'role cashback_domain kept: it still holds privileges outside this database, or this role may not drop it';
end;
$$;
