-- Additive Tools storage only. Review and back up before applying to an existing gateway.
-- No native task, API wallet, token or historical execution rows are imported.
CREATE TABLE tools_executions (
  id TEXT PRIMARY KEY, user_id TEXT, agent_id TEXT, session_id TEXT,
  model TEXT, request_hash TEXT, revision TEXT, binding_json TEXT NOT NULL,
  status TEXT, upstream_id TEXT, error_code TEXT, native_output TEXT,
  output TEXT, manifest TEXT, cancel_attempted NUMERIC,
  lease_owner TEXT, lease_until INTEGER, created_at INTEGER, updated_at INTEGER
);
CREATE INDEX idx_tools_executions_user_id ON tools_executions(user_id);
CREATE TABLE tools_model_bindings (
  model TEXT PRIMARY KEY, enabled NUMERIC, capability TEXT,
  binding_json TEXT NOT NULL, source_json TEXT NOT NULL, revision TEXT NOT NULL
);
CREATE TABLE tools_request_nonces (nonce TEXT PRIMARY KEY, expires_at INTEGER);
CREATE INDEX idx_tools_request_nonces_expires_at ON tools_request_nonces(expires_at);
