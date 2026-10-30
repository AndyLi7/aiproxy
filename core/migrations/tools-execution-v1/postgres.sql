-- Additive Tools storage only. Apply as a reviewed migration, never during HTTP requests.
-- Inspect an existing partial installation instead of hiding it with IF NOT EXISTS.
BEGIN;
CREATE TABLE tools_executions (
  id VARCHAR(186) PRIMARY KEY, user_id VARCHAR(200), agent_id VARCHAR(200), session_id VARCHAR(200),
  model VARCHAR(256), request_hash VARCHAR(64), revision VARCHAR(64), binding_json TEXT NOT NULL,
  status VARCHAR(40), upstream_id VARCHAR(200), error_code VARCHAR(100), native_output TEXT,
  output TEXT, manifest TEXT, cancel_attempted BOOLEAN,
  lease_owner VARCHAR(64), lease_until BIGINT, created_at BIGINT, updated_at BIGINT
);
CREATE INDEX idx_tools_executions_user_id ON tools_executions(user_id);
CREATE TABLE tools_model_bindings (
  model VARCHAR(256) PRIMARY KEY, enabled BOOLEAN, capability VARCHAR(80),
  binding_json TEXT NOT NULL, source_json TEXT NOT NULL, revision VARCHAR(64) NOT NULL
);
CREATE TABLE tools_request_nonces (nonce VARCHAR(64) PRIMARY KEY, expires_at BIGINT);
CREATE INDEX idx_tools_request_nonces_expires_at ON tools_request_nonces(expires_at);
COMMIT;
