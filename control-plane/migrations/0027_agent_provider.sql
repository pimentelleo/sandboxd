-- Instance-wide task provider selected from Settings -> Agents. This is safe
-- operator configuration, not a credential. Existing installations retain an
-- empty value so startup can fall back to SANDBOXD_DEFAULT_AGENT.
ALTER TABLE instance_settings ADD COLUMN agent_provider TEXT NOT NULL DEFAULT '';
