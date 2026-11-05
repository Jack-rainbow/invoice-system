-- Shared, bounded authentication throttles. No raw passwords, tokens or IPs.
CREATE TABLE auth_rate_limits (
 scope text NOT NULL CHECK(length(scope) BETWEEN 1 AND 64),
 key_hash bytea NOT NULL CHECK(octet_length(key_hash)=32),
 bucket bigint NOT NULL, hits integer NOT NULL CHECK(hits BETWEEN 1 AND 10000),
 expires_at timestamptz NOT NULL,
 PRIMARY KEY(scope,key_hash,bucket)
);
CREATE INDEX auth_rate_expiry ON auth_rate_limits(expires_at);
