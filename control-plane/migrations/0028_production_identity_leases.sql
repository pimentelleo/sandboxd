-- Additive local compatibility for the production identity and coordination
-- primitives. Existing owner_token and external_* semantics remain unchanged.
CREATE TABLE principal (
    id           TEXT PRIMARY KEY,
    provider     TEXT NOT NULL,
    tenant_id    TEXT NOT NULL,
    subject      TEXT NOT NULL,
    display_name TEXT,
    email        TEXT,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    UNIQUE (provider, tenant_id, subject)
);

ALTER TABLE sandbox ADD COLUMN owner_principal_id TEXT;
ALTER TABLE app ADD COLUMN owner_principal_id TEXT;
ALTER TABLE snapshot ADD COLUMN owner_principal_id TEXT;
CREATE INDEX sandbox_owner_principal_idx ON sandbox(owner_principal_id);
CREATE INDEX app_owner_principal_idx ON app(owner_principal_id);
CREATE INDEX snapshot_owner_principal_idx ON snapshot(owner_principal_id);

-- This binding survives DELETE /sandbox/{id}, just like workspace_owner, so
-- identity-scoped workspace reuse is safe. Purge removes it.
CREATE TABLE workspace_principal_owner (
    sandbox_id   TEXT PRIMARY KEY,
    principal_id TEXT NOT NULL,
    created_at   INTEGER NOT NULL
);
CREATE INDEX workspace_principal_owner_principal_idx ON workspace_principal_owner(principal_id);

CREATE TABLE browser_session (
    token_hash   TEXT PRIMARY KEY,
    principal_id TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    revoked_at   INTEGER
);
CREATE INDEX browser_session_principal_idx ON browser_session(principal_id);
CREATE INDEX browser_session_expires_idx ON browser_session(expires_at);

-- OAuth state and PKCE verifier material is hashed/encrypted by the auth
-- layer before it reaches this table. This store never accepts plaintext.
CREATE TABLE login_transaction (
    id                   TEXT PRIMARY KEY,
    provider             TEXT NOT NULL,
    state_hash           TEXT NOT NULL UNIQUE,
    nonce_hash           TEXT NOT NULL,
    verifier_ciphertext  BLOB NOT NULL,
    verifier_nonce       BLOB NOT NULL,
    redirect_uri         TEXT NOT NULL,
    created_at           INTEGER NOT NULL,
    expires_at           INTEGER NOT NULL,
    consumed_at          INTEGER
);
CREATE INDEX login_transaction_expiry_idx ON login_transaction(expires_at);

CREATE TABLE operation_lease (
    resource_type TEXT NOT NULL,
    resource_id   TEXT NOT NULL,
    holder_id     TEXT NOT NULL,
    token         TEXT NOT NULL,
    acquired_at   INTEGER NOT NULL,
    heartbeat_at  INTEGER NOT NULL,
    expires_at    INTEGER NOT NULL,
    PRIMARY KEY (resource_type, resource_id)
);
CREATE INDEX operation_lease_expiry_idx ON operation_lease(expires_at);
