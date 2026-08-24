-- Everything that could tell us the `public` schema was touched: relations,
-- routines, types, and the schema every installed extension landed in.
--
-- Extensions are in here because they are the quiet way a component takes
-- over `public`: `CREATE EXTENSION pgcrypto` with no SCHEMA clause installs
-- into the first writable schema on the search path, and nobody notices
-- until two components want different versions of it.
SELECT 'relation ' || c.relkind || ' ' || c.relname
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'public'
UNION ALL
SELECT 'routine ' || p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')'
  FROM pg_proc p
  JOIN pg_namespace n ON n.oid = p.pronamespace
 WHERE n.nspname = 'public'
UNION ALL
SELECT 'type ' || t.typname
  FROM pg_type t
  JOIN pg_namespace n ON n.oid = t.typnamespace
 WHERE n.nspname = 'public'
UNION ALL
SELECT 'extension ' || e.extname || ' in ' || n.nspname
  FROM pg_extension e
  JOIN pg_namespace n ON n.oid = e.extnamespace
 ORDER BY 1;
