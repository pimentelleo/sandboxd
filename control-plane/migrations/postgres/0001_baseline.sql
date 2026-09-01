-- Clean PostgreSQL baseline. This is intentionally independent from the
-- additive SQLite history and is applied only by MigratePostgres.
CREATE TABLE principal (
    id TEXT PRIMARY KEY,
    provider TEXT NOT NULL,
    tenant_id TEXT NOT NULL,
    subject TEXT NOT NULL,
    display_name TEXT,
    email TEXT,
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,
    UNIQUE (provider, tenant_id, subject)
);

CREATE TABLE sandbox (
    id TEXT PRIMARY KEY,
    status TEXT NOT NULL,
    image TEXT NOT NULL,
    workspace_img TEXT NOT NULL,
    workspace_mnt TEXT NOT NULL,
    container_id TEXT,
    cgroup_path TEXT,
    memory_high TEXT NOT NULL DEFAULT '4G',
    error_message TEXT,
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,
    last_active_at BIGINT NOT NULL DEFAULT 0,
    stopped_at BIGINT,
    keepalive_until BIGINT,
    container_ip TEXT,
    external_user_id TEXT,
    external_project_id TEXT,
    external_workspace_id TEXT,
    visibility TEXT NOT NULL DEFAULT 'public',
    idle_policy TEXT NOT NULL DEFAULT 'sleep',
    app_id TEXT,
    web_port INTEGER,
    git_remote_url TEXT,
    owner_principal_id TEXT REFERENCES principal(id)
);
CREATE INDEX sandbox_status_idx ON sandbox(status);
CREATE INDEX sandbox_last_active_idx ON sandbox(status, last_active_at);
CREATE INDEX sandbox_container_ip_idx ON sandbox(container_ip);
CREATE INDEX sandbox_external_user_idx ON sandbox(external_user_id);
CREATE INDEX sandbox_external_project_idx ON sandbox(external_project_id);
CREATE INDEX idx_sandbox_app_id ON sandbox(app_id);
CREATE INDEX sandbox_owner_principal_idx ON sandbox(owner_principal_id);

CREATE TABLE sandbox_port (
    sandbox_id TEXT NOT NULL REFERENCES sandbox(id) ON DELETE CASCADE,
    port INTEGER NOT NULL,
    PRIMARY KEY (sandbox_id, port)
);

CREATE TABLE workspace_owner (
    sandbox_id TEXT PRIMARY KEY,
    external_user_id TEXT NOT NULL,
    external_project_id TEXT,
    external_workspace_id TEXT,
    created_at BIGINT NOT NULL
);
CREATE INDEX workspace_owner_external_user_idx ON workspace_owner(external_user_id);
CREATE INDEX workspace_owner_external_project_idx ON workspace_owner(external_project_id);

CREATE TABLE workspace_principal_owner (
    sandbox_id TEXT PRIMARY KEY,
    principal_id TEXT NOT NULL REFERENCES principal(id),
    created_at BIGINT NOT NULL
);
CREATE INDEX workspace_principal_owner_principal_idx ON workspace_principal_owner(principal_id);

CREATE TABLE audit_log (
    id BIGSERIAL PRIMARY KEY,
    at BIGINT NOT NULL,
    actor_kind TEXT NOT NULL,
    actor_name TEXT,
    actor_ip TEXT,
    external_user_id TEXT,
    action TEXT NOT NULL,
    target TEXT,
    detail TEXT
);
CREATE INDEX audit_log_at_idx ON audit_log(at);
CREATE INDEX audit_log_action_idx ON audit_log(action);
CREATE INDEX audit_log_external_user_idx ON audit_log(external_user_id);

CREATE TABLE task (
    task_id TEXT PRIMARY KEY,
    sandbox_id TEXT NOT NULL,
    external_user_id TEXT,
    external_project_id TEXT,
    agent TEXT NOT NULL,
    prompt TEXT NOT NULL,
    status TEXT NOT NULL,
    result_json TEXT,
    created_at BIGINT NOT NULL,
    finished_at BIGINT,
    timeout_s INTEGER NOT NULL DEFAULT 0,
    execution_kind TEXT NOT NULL DEFAULT 'runtimed',
    conversation_id TEXT,
    conversation_turn_id TEXT
);
CREATE INDEX task_sandbox_idx ON task(sandbox_id);
CREATE INDEX task_execution_kind_status_idx ON task(execution_kind, status);
CREATE INDEX task_conversation_turn_idx ON task(conversation_turn_id);

CREATE TABLE app (
    id TEXT PRIMARY KEY,
    owner_token TEXT NOT NULL,
    owner_principal_id TEXT REFERENCES principal(id),
    external_user_id TEXT,
    external_project_id TEXT,
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    tags TEXT NOT NULL DEFAULT '[]',
    latest_snapshot_id TEXT,
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,
    runtime_preset TEXT,
    git_repo_url TEXT,
    git_branch TEXT,
    git_credential_id TEXT,
    last_import_at BIGINT
);
CREATE INDEX idx_app_owner ON app(owner_token);
CREATE INDEX app_owner_principal_idx ON app(owner_principal_id);

CREATE TABLE snapshot (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    owner_token TEXT NOT NULL,
    owner_principal_id TEXT REFERENCES principal(id),
    source_sandbox_id TEXT,
    created_by_user_id TEXT,
    base_image TEXT NOT NULL,
    visibility TEXT NOT NULL DEFAULT 'private',
    format TEXT NOT NULL DEFAULT 'raw',
    status TEXT NOT NULL,
    image_path TEXT NOT NULL,
    size_bytes BIGINT,
    error_message TEXT,
    created_at BIGINT NOT NULL,
    source_app_id TEXT
);
CREATE INDEX snapshot_owner_idx ON snapshot(owner_token);
CREATE INDEX idx_snapshot_app ON snapshot(source_app_id);
CREATE INDEX snapshot_owner_principal_idx ON snapshot(owner_principal_id);

CREATE TABLE app_config (
    id TEXT PRIMARY KEY,
    app_id TEXT NOT NULL,
    key TEXT NOT NULL,
    value_ciphertext BYTEA,
    value_nonce BYTEA,
    value_plaintext TEXT,
    sensitive INTEGER NOT NULL DEFAULT 0,
    access_policy TEXT NOT NULL DEFAULT 'control_plane_only',
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,
    UNIQUE (app_id, key)
);
CREATE INDEX idx_app_config_app ON app_config(app_id);

CREATE TABLE app_events (
    id TEXT PRIMARY KEY,
    owner_token TEXT NOT NULL,
    app_id TEXT,
    sandbox_id TEXT,
    task_id TEXT,
    snapshot_id TEXT,
    type TEXT NOT NULL,
    severity TEXT NOT NULL,
    message TEXT NOT NULL,
    payload_json TEXT,
    created_at TEXT NOT NULL
);
CREATE INDEX idx_app_events_app ON app_events(owner_token, app_id, id);
CREATE INDEX idx_app_events_task ON app_events(owner_token, task_id, id);

CREATE TABLE instance_settings (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    idle_reap_enabled INTEGER NOT NULL,
    idle_threshold_seconds INTEGER NOT NULL,
    keepalive_max_seconds INTEGER NOT NULL,
    updated_at BIGINT NOT NULL,
    agent_default_models TEXT NOT NULL DEFAULT '{}',
    agent_provider TEXT NOT NULL DEFAULT ''
);

CREATE TABLE git_credential (
    id TEXT PRIMARY KEY,
    owner_token TEXT NOT NULL,
    name TEXT NOT NULL,
    host TEXT NOT NULL DEFAULT '',
    username TEXT NOT NULL DEFAULT '',
    secret_enc BYTEA NOT NULL,
    secret_nonce BYTEA NOT NULL,
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL
);
CREATE INDEX idx_git_credential_owner ON git_credential(owner_token);
CREATE UNIQUE INDEX idx_git_credential_owner_name ON git_credential(owner_token, name);

CREATE TABLE console_auth (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    password_hash TEXT NOT NULL,
    updated_at BIGINT NOT NULL
);
CREATE TABLE console_session (
    token_hash TEXT PRIMARY KEY,
    owner_token TEXT NOT NULL,
    created_at BIGINT NOT NULL,
    last_used_at BIGINT NOT NULL,
    expires_at BIGINT NOT NULL
);
CREATE INDEX console_session_expires_idx ON console_session(expires_at);
CREATE TABLE api_key (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    key_hash TEXT NOT NULL UNIQUE,
    prefix TEXT NOT NULL,
    created_at BIGINT NOT NULL,
    last_used_at BIGINT
);
CREATE UNIQUE INDEX api_key_name_idx ON api_key(name);

CREATE TABLE conversation (
    id TEXT PRIMARY KEY,
    sandbox_id TEXT NOT NULL,
    agent TEXT NOT NULL,
    state TEXT NOT NULL,
    default_mode TEXT NOT NULL,
    active_turn_id TEXT,
    last_error TEXT,
    next_sequence BIGINT NOT NULL DEFAULT 0,
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,
    archived_at BIGINT
);
CREATE UNIQUE INDEX conversation_one_active_sandbox_idx ON conversation(sandbox_id) WHERE archived_at IS NULL;
CREATE INDEX conversation_sandbox_state_idx ON conversation(sandbox_id, state);

CREATE TABLE conversation_turn (
    id TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL REFERENCES conversation(id) ON DELETE CASCADE,
    task_id TEXT NOT NULL UNIQUE,
    sequence BIGINT NOT NULL,
    prompt TEXT NOT NULL,
    mode TEXT NOT NULL,
    status TEXT NOT NULL,
    error_message TEXT,
    created_at BIGINT NOT NULL,
    started_at BIGINT,
    finished_at BIGINT,
    model TEXT NOT NULL DEFAULT '',
    reasoning_effort TEXT NOT NULL DEFAULT '',
    context_tier TEXT NOT NULL DEFAULT 'default'
);
CREATE INDEX conversation_turn_queue_idx ON conversation_turn(conversation_id, status, sequence);

CREATE TABLE conversation_message (
    id TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL REFERENCES conversation(id) ON DELETE CASCADE,
    turn_id TEXT NOT NULL REFERENCES conversation_turn(id) ON DELETE CASCADE,
    sequence BIGINT NOT NULL,
    role TEXT NOT NULL,
    content TEXT NOT NULL,
    status TEXT NOT NULL,
    created_at BIGINT NOT NULL
);
CREATE INDEX conversation_message_timeline_idx ON conversation_message(conversation_id, sequence);

CREATE TABLE conversation_interaction (
    id TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL REFERENCES conversation(id) ON DELETE CASCADE,
    turn_id TEXT NOT NULL REFERENCES conversation_turn(id) ON DELETE CASCADE,
    sequence BIGINT NOT NULL,
    type TEXT NOT NULL,
    status TEXT NOT NULL,
    provider_request_id TEXT NOT NULL,
    question TEXT NOT NULL DEFAULT '',
    choices_json TEXT NOT NULL DEFAULT '[]',
    allow_freeform INTEGER NOT NULL DEFAULT 0,
    summary TEXT NOT NULL DEFAULT '',
    plan TEXT NOT NULL DEFAULT '',
    actions_json TEXT NOT NULL DEFAULT '[]',
    recommended_action TEXT NOT NULL DEFAULT '',
    answer TEXT,
    approved INTEGER,
    selected_action TEXT,
    feedback TEXT,
    created_at BIGINT NOT NULL,
    resolved_at BIGINT
);
CREATE UNIQUE INDEX conversation_interaction_provider_request_idx ON conversation_interaction(conversation_id, provider_request_id);
CREATE UNIQUE INDEX conversation_one_pending_interaction_idx ON conversation_interaction(conversation_id) WHERE status = 'pending';
CREATE INDEX conversation_interaction_timeline_idx ON conversation_interaction(conversation_id, sequence);

CREATE TABLE conversation_event (
    id BIGSERIAL PRIMARY KEY,
    conversation_id TEXT NOT NULL REFERENCES conversation(id) ON DELETE CASCADE,
    turn_id TEXT REFERENCES conversation_turn(id) ON DELETE CASCADE,
    type TEXT NOT NULL,
    payload_json TEXT NOT NULL,
    created_at BIGINT NOT NULL
);
CREATE INDEX conversation_event_cursor_idx ON conversation_event(conversation_id, id);

CREATE TABLE conversation_child (
    id TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL REFERENCES conversation(id) ON DELETE CASCADE,
    parent_turn_id TEXT NOT NULL REFERENCES conversation_turn(id) ON DELETE CASCADE,
    label TEXT NOT NULL DEFAULT '',
    prompt TEXT NOT NULL,
    model TEXT NOT NULL DEFAULT '',
    reasoning_effort TEXT NOT NULL DEFAULT '',
    context_tier TEXT NOT NULL DEFAULT 'default',
    status TEXT NOT NULL,
    workspace_path TEXT NOT NULL,
    worker_container TEXT NOT NULL DEFAULT '',
    result TEXT NOT NULL DEFAULT '',
    error_message TEXT,
    patch_state TEXT NOT NULL DEFAULT 'none',
    patch_json TEXT NOT NULL DEFAULT '',
    created_at BIGINT NOT NULL,
    started_at BIGINT,
    finished_at BIGINT
);
CREATE INDEX conversation_child_timeline_idx ON conversation_child(conversation_id, created_at);
CREATE INDEX conversation_child_active_idx ON conversation_child(status, created_at);

CREATE TABLE browser_session (
    token_hash TEXT PRIMARY KEY,
    principal_id TEXT NOT NULL REFERENCES principal(id),
    created_at BIGINT NOT NULL,
    last_used_at BIGINT NOT NULL,
    expires_at BIGINT NOT NULL,
    revoked_at BIGINT
);
CREATE INDEX browser_session_principal_idx ON browser_session(principal_id);
CREATE INDEX browser_session_expires_idx ON browser_session(expires_at);

CREATE TABLE login_transaction (
    id TEXT PRIMARY KEY,
    provider TEXT NOT NULL,
    state_hash TEXT NOT NULL UNIQUE,
    nonce_hash TEXT NOT NULL,
    verifier_ciphertext BYTEA NOT NULL,
    verifier_nonce BYTEA NOT NULL,
    redirect_uri TEXT NOT NULL,
    created_at BIGINT NOT NULL,
    expires_at BIGINT NOT NULL,
    consumed_at BIGINT
);
CREATE INDEX login_transaction_expiry_idx ON login_transaction(expires_at);

CREATE TABLE operation_lease (
    resource_type TEXT NOT NULL,
    resource_id TEXT NOT NULL,
    holder_id TEXT NOT NULL,
    token TEXT NOT NULL,
    acquired_at BIGINT NOT NULL,
    heartbeat_at BIGINT NOT NULL,
    expires_at BIGINT NOT NULL,
    PRIMARY KEY (resource_type, resource_id)
);
CREATE INDEX operation_lease_expiry_idx ON operation_lease(expires_at);
