-- Initial schema for llmgw.
--
-- Conventions:
--   * timestamps are INTEGER unix milliseconds (UTC)
--   * durations are INTEGER milliseconds; NULL means "inherit from the upper level"
--     (global settings -> provider -> alias); 0 means "unlimited"
--   * booleans are INTEGER 0/1
--   * JSON values are stored as TEXT

CREATE TABLE admins (
    id            INTEGER PRIMARY KEY,
    username      TEXT    NOT NULL UNIQUE,
    password_hash TEXT    NOT NULL,          -- argon2id PHC string
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);

CREATE TABLE admin_sessions (
    id           TEXT    PRIMARY KEY,        -- SHA-256 (hex) of the session token
    admin_id     INTEGER NOT NULL REFERENCES admins(id) ON DELETE CASCADE,
    csrf_token   TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL
);
CREATE INDEX admin_sessions_expires_at ON admin_sessions(expires_at);

-- Dynamic settings edited from the admin UI. value is JSON.
CREATE TABLE settings (
    key        TEXT    PRIMARY KEY,
    value      TEXT    NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE providers (
    id                 INTEGER PRIMARY KEY,
    name               TEXT    NOT NULL UNIQUE,
    type               TEXT    NOT NULL CHECK (type IN ('ollama', 'openai_compatible')),
    base_url           TEXT    NOT NULL,
    api_key_enc        BLOB,                 -- AES-GCM(nonce || ciphertext), NULL if none
    supports_anthropic INTEGER NOT NULL DEFAULT 0,
    connect_timeout_ms INTEGER,
    queue_timeout_ms   INTEGER,
    ttft_timeout_ms    INTEGER,              -- provider-level default for aliases
    idle_timeout_ms    INTEGER,
    max_duration_ms    INTEGER,
    max_concurrency    INTEGER,              -- NULL/0 = unlimited
    enabled            INTEGER NOT NULL DEFAULT 1,
    -- health / discovery state (written by the health checker)
    health_status      TEXT,                 -- 'ok' | 'error' | NULL (unknown)
    health_error       TEXT,
    health_checked_at  INTEGER,
    models_json        TEXT,                 -- last fetched upstream model list
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL
);

CREATE TABLE aliases (
    id                       INTEGER PRIMARY KEY,
    name                     TEXT    NOT NULL UNIQUE, -- public model name (may contain ':')
    enabled                  INTEGER NOT NULL DEFAULT 1,
    description              TEXT    NOT NULL DEFAULT '',
    ttft_timeout_ms          INTEGER,
    idle_timeout_ms          INTEGER,
    max_duration_ms          INTEGER,
    fallback_on_ttft_timeout INTEGER,        -- NULL = inherit (global default false)
    -- {"defaults": {...}, "overrides": {...},
    --  "system_prompt": {"mode": "prepend"|"replace", "text": "..."}}
    params_json              TEXT    NOT NULL DEFAULT '{}',
    created_at               INTEGER NOT NULL,
    updated_at               INTEGER NOT NULL
);

CREATE TABLE alias_targets (
    id             INTEGER PRIMARY KEY,
    alias_id       INTEGER NOT NULL REFERENCES aliases(id) ON DELETE CASCADE,
    provider_id    INTEGER NOT NULL REFERENCES providers(id) ON DELETE RESTRICT,
    upstream_model TEXT    NOT NULL,
    priority       INTEGER NOT NULL DEFAULT 0 -- lower value is tried first
);
CREATE INDEX alias_targets_alias ON alias_targets(alias_id, priority);
CREATE INDEX alias_targets_provider ON alias_targets(provider_id);

CREATE TABLE virtual_keys (
    id                    INTEGER PRIMARY KEY,
    name                  TEXT    NOT NULL,
    key_prefix            TEXT    NOT NULL,  -- display only, e.g. "sk-gw-abcd"
    key_hash              TEXT    NOT NULL UNIQUE, -- SHA-256 hex of the plaintext key
    -- rotation grace period: the previous key stays valid until prev_key_expires_at
    prev_key_hash         TEXT    UNIQUE,
    prev_key_expires_at   INTEGER,
    status                TEXT    NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    allowed_aliases_json  TEXT    NOT NULL DEFAULT '[]', -- empty = all aliases
    expires_at            INTEGER,
    log_mode              TEXT    NOT NULL DEFAULT 'full' CHECK (log_mode IN ('full', 'metadata_only', 'none')),
    max_concurrency       INTEGER,
    rpm_limit             INTEGER,
    nonstream_keepalive   INTEGER NOT NULL DEFAULT 0,
    imported              INTEGER NOT NULL DEFAULT 0, -- hash imported from LiteLLM
    last_used_at          INTEGER,
    created_at            INTEGER NOT NULL,
    updated_at            INTEGER NOT NULL
);

CREATE TABLE request_logs (
    id                    INTEGER PRIMARY KEY,
    created_at            INTEGER NOT NULL,
    key_id                INTEGER,           -- no FK: keys may be deleted
    key_name              TEXT,              -- snapshot
    alias                 TEXT,
    provider_id           INTEGER,
    upstream_model        TEXT,
    endpoint              TEXT    NOT NULL,
    inbound_format        TEXT    NOT NULL,  -- openai | ollama | anthropic
    upstream_format       TEXT,
    passthrough           INTEGER NOT NULL DEFAULT 0,
    stream                INTEGER NOT NULL DEFAULT 0,
    status_code           INTEGER NOT NULL,
    error                 TEXT,
    queue_ms              INTEGER,
    latency_ms            INTEGER,
    ttft_ms               INTEGER,
    timeout_phase         TEXT,              -- queue | connect | ttft | idle | max_duration
    cancelled_by_client   INTEGER NOT NULL DEFAULT 0,
    interrupted           INTEGER NOT NULL DEFAULT 0,
    attempts_json         TEXT,
    prompt_tokens         INTEGER,
    completion_tokens     INTEGER,
    client_ip             TEXT,
    user_agent            TEXT,
    request_body          TEXT,
    response_body         TEXT,
    upstream_request_body TEXT,
    body_truncated        INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX request_logs_created_at ON request_logs(created_at);
CREATE INDEX request_logs_key ON request_logs(key_id, created_at);
CREATE INDEX request_logs_alias ON request_logs(alias, created_at);
CREATE INDEX request_logs_status ON request_logs(status_code, created_at);

CREATE TABLE audit_logs (
    id          INTEGER PRIMARY KEY,
    created_at  INTEGER NOT NULL,
    actor       TEXT    NOT NULL,
    action      TEXT    NOT NULL,            -- e.g. key.create, provider.update, login.failure
    target_type TEXT,
    target_id   TEXT,
    detail_json TEXT,
    client_ip   TEXT
);
CREATE INDEX audit_logs_created_at ON audit_logs(created_at);
