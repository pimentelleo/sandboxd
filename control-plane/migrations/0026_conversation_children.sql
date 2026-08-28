-- Isolated background workers delegated by a hosted Copilot conversation.
-- Their SDK sessions and containers are control-plane resources; a worker
-- never receives the parent workspace or any provider credential.
PRAGMA foreign_keys = ON;

CREATE TABLE conversation_child (
    id               TEXT PRIMARY KEY,
    conversation_id  TEXT NOT NULL REFERENCES conversation(id) ON DELETE CASCADE,
    parent_turn_id   TEXT NOT NULL REFERENCES conversation_turn(id) ON DELETE CASCADE,
    label            TEXT NOT NULL DEFAULT '',
    prompt           TEXT NOT NULL,
    model            TEXT NOT NULL DEFAULT '',
    reasoning_effort TEXT NOT NULL DEFAULT '',
    context_tier     TEXT NOT NULL DEFAULT 'default',
    status           TEXT NOT NULL, -- queued | preparing | running | cancelling | succeeded | failed | cancelled | interrupted
    workspace_path   TEXT NOT NULL,
    worker_container TEXT NOT NULL DEFAULT '',
    result           TEXT NOT NULL DEFAULT '',
    error_message    TEXT,
    patch_state      TEXT NOT NULL DEFAULT 'none', -- none | available | unavailable
    patch_json       TEXT NOT NULL DEFAULT '',
    created_at       INTEGER NOT NULL,
    started_at       INTEGER,
    finished_at      INTEGER
);
CREATE INDEX conversation_child_timeline_idx
    ON conversation_child(conversation_id, created_at);
CREATE INDEX conversation_child_active_idx
    ON conversation_child(status, created_at);
