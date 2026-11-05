CREATE TABLE credit_accounts (
 user_id uuid PRIMARY KEY REFERENCES users(id), available bigint NOT NULL DEFAULT 0, frozen bigint NOT NULL DEFAULT 0,
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
 CONSTRAINT ck_wallet_nonnegative CHECK(available>=0 AND frozen>=0)
);
CREATE TABLE credit_entries (
 id uuid PRIMARY KEY, account_id uuid NOT NULL REFERENCES credit_accounts(user_id),
 kind text NOT NULL CHECK(kind IN ('GRANT','RESERVE','CONSUME','RELEASE','ADJUST')),
 delta_available bigint NOT NULL, delta_frozen bigint NOT NULL,
 before_available bigint NOT NULL CHECK(before_available>=0), after_available bigint NOT NULL CHECK(after_available>=0),
 before_frozen bigint NOT NULL CHECK(before_frozen>=0), after_frozen bigint NOT NULL CHECK(after_frozen>=0),
 operation_id uuid NOT NULL, related_operation_id uuid, actor_id uuid NOT NULL REFERENCES users(id), reason text NOT NULL CHECK(btrim(reason)<>''),
 created_at timestamptz NOT NULL DEFAULT now(),
 CHECK(after_available::numeric=before_available::numeric+delta_available::numeric AND after_frozen::numeric=before_frozen::numeric+delta_frozen::numeric),
 CHECK((kind='GRANT' AND delta_available>0 AND delta_frozen=0) OR
 (kind='RESERVE' AND delta_available<0 AND delta_frozen=-delta_available::numeric) OR
 (kind='CONSUME' AND delta_available=0 AND delta_frozen<0) OR
 (kind='RELEASE' AND delta_available>0 AND delta_frozen=-delta_available::numeric) OR kind='ADJUST')
);
CREATE UNIQUE INDEX uq_ledger_operation ON credit_entries(operation_id,kind);
CREATE INDEX credit_entries_account_time ON credit_entries(account_id,created_at,id);
CREATE TRIGGER ledger_immutable BEFORE UPDATE OR DELETE ON credit_entries FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();
-- Deployment must give the separate application role INSERT/SELECT only; no role is created globally here.
REVOKE UPDATE, DELETE, TRUNCATE ON credit_entries FROM PUBLIC;
CREATE TABLE idempotency_records (
 id uuid PRIMARY KEY, actor_id uuid NOT NULL REFERENCES users(id), operation text NOT NULL CHECK(operation<>''),
 key_hash bytea NOT NULL CHECK(octet_length(key_hash)>0), request_hash bytea NOT NULL CHECK(octet_length(request_hash)>0),
 resource_id uuid, result_json jsonb, status text NOT NULL CHECK(status IN ('PENDING','COMPLETED','FAILED')),
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
 CHECK(status<>'COMPLETED' OR (resource_id IS NOT NULL AND result_json IS NOT NULL))
);
CREATE UNIQUE INDEX uq_idem ON idempotency_records(actor_id,operation,key_hash);
