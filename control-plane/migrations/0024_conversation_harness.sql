-- Durable hosted GitHub Copilot conversations. The control plane, rather than
-- the sandbox, owns every record here so a pending user interaction survives a
-- browser disconnect and a sandbox stop/start cycle.
PRAGMA foreign_keys = ON;

-- Legacy runtimed tasks retain their default kind. Hosted conversation turns
-- use their own kind so boot reconciliation never tries to attach a runtimed
-- event watcher to a control-plane-owned SDK session.
ALTER TABLE task ADD COLUMN execution_kind TEXT NOT NULL DEFAULT 'runtimed';
ALTER TABLE task ADD COLUMN conversation_id TEXT;
ALTER TABLE task ADD COLUMN conversation_turn_id TEXT;
CREATE INDEX task_execution_kind_status_idx ON task(execution_kind, status);
CREATE INDEX task_conversation_turn_idx ON task(conversation_turn_id);

CREATE TABLE conversation (
    id             TEXT PRIMARY KEY,
    sandbox_id     TEXT NOT NULL,
    agent          TEXT NOT NULL,
    state          TEXT NOT NULL, -- idle | running | waiting_input | waiting_plan | failed | archived
    default_mode   TEXT NOT NULL, -- interactive | plan | autopilot
    active_turn_id TEXT,
    last_error     TEXT,
    next_sequence  INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    archived_at    INTEGER
);
-- There is exactly one current conversational context per sandbox. Historical
-- transcripts stay archived until the sandbox is irreversibly purged.
CREATE UNIQUE INDEX conversation_one_active_sandbox_idx
    ON conversation(sandbox_id) WHERE archived_at IS NULL;
CREATE INDEX conversation_sandbox_state_idx ON conversation(sandbox_id, state);

CREATE TABLE conversation_turn (
    id              TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL REFERENCES conversation(id) ON DELETE CASCADE,
    task_id         TEXT NOT NULL UNIQUE,
    sequence        INTEGER NOT NULL,
    prompt          TEXT NOT NULL,
    mode            TEXT NOT NULL,
    status          TEXT NOT NULL, -- queued | running | waiting_input | waiting_plan | cancelling | succeeded | failed | cancelled
    error_message   TEXT,
    created_at      INTEGER NOT NULL,
    started_at      INTEGER,
    finished_at     INTEGER
);
CREATE INDEX conversation_turn_queue_idx
    ON conversation_turn(conversation_id, status, sequence);

CREATE TABLE conversation_message (
    id              TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL REFERENCES conversation(id) ON DELETE CASCADE,
    turn_id         TEXT NOT NULL REFERENCES conversation_turn(id) ON DELETE CASCADE,
    sequence        INTEGER NOT NULL,
    role            TEXT NOT NULL, -- user | assistant
    content         TEXT NOT NULL,
    status          TEXT NOT NULL, -- complete | streaming
    created_at      INTEGER NOT NULL
);
CREATE INDEX conversation_message_timeline_idx
    ON conversation_message(conversation_id, sequence);

CREATE TABLE conversation_interaction (
    id                  TEXT PRIMARY KEY,
    conversation_id     TEXT NOT NULL REFERENCES conversation(id) ON DELETE CASCADE,
    turn_id             TEXT NOT NULL REFERENCES conversation_turn(id) ON DELETE CASCADE,
    sequence            INTEGER NOT NULL,
    type                TEXT NOT NULL, -- user_input | plan
    status              TEXT NOT NULL, -- pending | resolved | interrupted
    provider_request_id TEXT NOT NULL,
    question            TEXT NOT NULL DEFAULT '',
    choices_json        TEXT NOT NULL DEFAULT '[]',
    allow_freeform      INTEGER NOT NULL DEFAULT 0,
    summary             TEXT NOT NULL DEFAULT '',
    plan                TEXT NOT NULL DEFAULT '',
    actions_json        TEXT NOT NULL DEFAULT '[]',
    recommended_action  TEXT NOT NULL DEFAULT '',
    answer              TEXT,
    approved            INTEGER,
    selected_action     TEXT,
    feedback            TEXT,
    created_at          INTEGER NOT NULL,
    resolved_at         INTEGER
);
CREATE UNIQUE INDEX conversation_interaction_provider_request_idx
    ON conversation_interaction(conversation_id, provider_request_id);
-- Native SDK interaction callbacks are sequential. Enforcing that invariant in
-- the durable store makes duplicate/replayed responses safe.
CREATE UNIQUE INDEX conversation_one_pending_interaction_idx
    ON conversation_interaction(conversation_id) WHERE status = 'pending';
CREATE INDEX conversation_interaction_timeline_idx
    ON conversation_interaction(conversation_id, sequence);

-- Events are the bounded replay cursor for a live console. Transcript rows
-- remain the canonical state; this table only closes the snapshot-to-SSE race.
CREATE TABLE conversation_event (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    conversation_id TEXT NOT NULL REFERENCES conversation(id) ON DELETE CASCADE,
    turn_id         TEXT REFERENCES conversation_turn(id) ON DELETE CASCADE,
    type            TEXT NOT NULL,
    payload_json    TEXT NOT NULL,
    created_at      INTEGER NOT NULL
);
CREATE INDEX conversation_event_cursor_idx ON conversation_event(conversation_id, id);
