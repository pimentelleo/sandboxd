CREATE TABLE preview_ticket (
    token_hash TEXT PRIMARY KEY,
    sandbox_id TEXT NOT NULL REFERENCES sandbox(id) ON DELETE CASCADE,
    principal_id TEXT NOT NULL REFERENCES principal(id),
    preview_host TEXT NOT NULL,
    admin_override BOOLEAN NOT NULL DEFAULT FALSE,
    created_at BIGINT NOT NULL,
    expires_at BIGINT NOT NULL,
    consumed_at BIGINT
);
CREATE INDEX preview_ticket_expiry_idx ON preview_ticket(expires_at);

CREATE TABLE preview_session (
    token_hash TEXT PRIMARY KEY,
    sandbox_id TEXT NOT NULL REFERENCES sandbox(id) ON DELETE CASCADE,
    principal_id TEXT NOT NULL REFERENCES principal(id),
    preview_host TEXT NOT NULL,
    admin_override BOOLEAN NOT NULL DEFAULT FALSE,
    created_at BIGINT NOT NULL,
    expires_at BIGINT NOT NULL,
    revoked_at BIGINT
);
CREATE INDEX preview_session_expiry_idx ON preview_session(expires_at);
