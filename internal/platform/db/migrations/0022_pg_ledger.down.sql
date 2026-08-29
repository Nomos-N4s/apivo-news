-- Reverses 0022_pg_ledger. Destroys the exit-route ledger and every
-- account, transfer and posting in it: never run against an environment
-- holding real money records.
--
-- The default privileges are un-declared in the same shape 0022 declared
-- them, or the entry outlives the schema in pg_default_acl. The revoke is
-- written to tolerate the role already being gone: cashback_domain is
-- cluster-wide and belongs to 0010, so another database's rollback can
-- have removed it between this statement and the last time anyone looked -
-- and for the same reason the role itself is deliberately NOT dropped
-- here.
do $$
begin
    execute 'alter default privileges in schema ledger revoke select, insert, update, delete on tables from cashback_domain';
    execute 'revoke usage on schema ledger from cashback_domain';
exception
    when undefined_object then
        raise notice 'role cashback_domain is no longer present in this cluster: its grants went with it';
end;
$$;

drop schema ledger cascade;
