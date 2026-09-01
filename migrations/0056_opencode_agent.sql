-- Admit the OpenCode session format. The agent column's CHECK is the announce
-- path's hard gate; parser.Agents and the OpenAPI enum grew the same name in
-- this change.
--
-- FORK CARRY-PATCH (weslandia): 'omp' is in this list and is not upstream's.
-- ADD CONSTRAINT validates every existing row, and this fork admitted OMP
-- sessions under its own 0055_agent_omp before upstream shipped OpenCode
-- natively. A deployment carrying OMP rows -- themis has 216 -- would fail this
-- statement and abort server startup, so the name has to be here rather than
-- wait for 0061_agent_omp to re-widen the constraint five migrations later.
--
-- 0061_agent_omp remains the fork's own declaration of the value. Keep both: if
-- a future upstream release rewrites this file, that rewrite lands without 'omp'
-- and 0061 is what puts it back. This is the same carry the fork already made
-- once, for 0054_cursor_grok_agents.
ALTER TABLE sessions DROP CONSTRAINT sessions_agent_check;
ALTER TABLE sessions ADD CONSTRAINT sessions_agent_check
  CHECK (agent IN ('claude', 'codex', 'pi', 'cursor', 'grok', 'opencode', 'omp'));
