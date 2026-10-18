-- Additive; no historical channels or bills are modified. Review before remote use.
CREATE TABLE IF NOT EXISTS channel_access_configurations (
 access_id varchar(128) PRIMARY KEY,
 request_hash varchar(64) NOT NULL,
 channel_id bigint NOT NULL,
 key_digest varchar(64) NOT NULL,
 identity_hash varchar(64) NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_channel_access_configurations_channel_id ON channel_access_configurations(channel_id);
