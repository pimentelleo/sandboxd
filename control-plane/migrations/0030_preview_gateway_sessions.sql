-- Opaque production preview bootstrap tickets and host-bound gateway sessions.
-- Plaintext credentials never reach durable storage.
CREATE TABLE preview_ticket (
    token_hash     TEXT PRIMARY KEY,
    sandbox_id     TEXT NOT NULL REFERENCES sandbox(id) ON DELETE CASCADE,
    principal_id   TEXT NOT NULL REFERENCES principal(id),
    preview_host   TEXT NOT NULL,
    admin_override INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL,
    expires_at     INTEGER NOT NULL,
    consumed_at    INTEGER
);
CREATE INDEX preview_ticket_expiry_idx ON preview_ticket(expires_at);

CREATE TABLE preview_session (
    token_hash     TEXT PRIMARY KEY,
    sandbox_id     TEXT NOT NULL REFERENCES sandbox(id) ON DELETE CASCADE,
    principal_id   TEXT NOT NULL REFERENCES principal(id),
    preview_host   TEXT NOT NULL,
    admin_override INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL,
    expires_at     INTEGER NOT NULL,
    revoked_at     INTEGER
);
CREATE INDEX preview_session_expiry_idx ON preview_session(expires_at);
