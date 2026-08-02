-- Admit OpenCode as a fourth agent format. The parser owns the enum
-- (parser.Agents), and announce derives its validation from it, so the only
-- schema-side gate is the sessions.agent CHECK constraint declared in 0001_init.
--
-- The constraint is dropped and re-added by its real name rather than by the
-- Postgres-generated default: 0001 wrote it inline, so its name is whatever the
-- server that first ran that migration chose ("sessions_agent_check" on a
-- vanilla Postgres, but a restored or renamed deployment can carry another).
-- Looking the name up from the catalog makes this migration safe on every
-- existing database instead of only the ones that got the default.
--
-- No parse.Epoch bump rides with this: adding a format changes nothing about
-- what an existing claude, codex, or pi session reduces to.
DO $$
DECLARE
  constraint_name text;
BEGIN
  SELECT con.conname
    INTO constraint_name
    FROM pg_constraint con
    JOIN pg_class rel ON rel.oid = con.conrelid
    JOIN pg_namespace nsp ON nsp.oid = rel.relnamespace
   WHERE rel.relname = 'sessions'
     AND nsp.nspname = current_schema()
     AND con.contype = 'c'
     AND pg_get_constraintdef(con.oid) ILIKE '%agent%'
     AND pg_get_constraintdef(con.oid) ILIKE '%claude%'
   LIMIT 1;

  IF constraint_name IS NOT NULL THEN
    EXECUTE format('ALTER TABLE sessions DROP CONSTRAINT %I', constraint_name);
  END IF;

  ALTER TABLE sessions
    ADD CONSTRAINT sessions_agent_check
    CHECK (agent IN ('claude', 'codex', 'pi', 'opencode'));
END
$$;
