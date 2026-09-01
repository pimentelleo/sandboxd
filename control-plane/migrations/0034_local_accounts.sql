-- Local multi-account authentication uses the durable principal/session model
-- shared with Entra while keeping password hashes separate from identity data.
CREATE TABLE local_account (
    principal_id  TEXT PRIMARY KEY,
    password_hash TEXT NOT NULL,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    FOREIGN KEY (principal_id) REFERENCES principal(id) ON DELETE CASCADE
);
