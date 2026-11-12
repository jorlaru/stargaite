-- Migration 001: base schema

CREATE TABLE providers (
    name        TEXT PRIMARY KEY,
    spec        TEXT NOT NULL,        -- JSON: protocol, base_url, api_key_env, billing, quota, cost_report, ...
    updated_at  INTEGER NOT NULL
);

CREATE TABLE targets (
    id                TEXT PRIMARY KEY,   -- 'provider/upstream-model'
    provider          TEXT NOT NULL REFERENCES providers(name),
    upstream_model    TEXT NOT NULL,
    spec              TEXT NOT NULL,      -- JSON: kind, capabilities, max_output_tokens, reasoning, pricing, ...
    verified_at       INTEGER,            -- last successful `targets verify`
    verified_version  TEXT,
    updated_at        INTEGER NOT NULL
);

CREATE TABLE models (
    name        TEXT PRIMARY KEY,         -- the alias
    spec        TEXT NOT NULL,            -- JSON: requires, reasoning, max_output_tokens, params, budget, limits, ...
    updated_at  INTEGER NOT NULL
);

CREATE TABLE model_targets (
    model     TEXT NOT NULL REFERENCES models(name) ON DELETE CASCADE,
    position  INTEGER NOT NULL,           -- 0 = primary, 1 and 2 = fallbacks
    target    TEXT NOT NULL REFERENCES targets(id),
    PRIMARY KEY (model, position)
);

CREATE TABLE keys (                       -- never deleted: a revoked key keeps its name and its usage
    name        TEXT PRIMARY KEY,
    spec        TEXT NOT NULL,            -- JSON: budget, limits, quota_share, trace_sample_rate, content_until
    created_at  INTEGER NOT NULL,
    revoked_at  INTEGER
);

CREATE TABLE key_models (
    key_name  TEXT NOT NULL REFERENCES keys(name) ON DELETE CASCADE,
    model     TEXT NOT NULL,              -- an alias, or '*'
    PRIMARY KEY (key_name, model)
);

CREATE TABLE key_secrets (                -- at most two rows per key, and two only during a rotation
    public_id    TEXT PRIMARY KEY,
    key_name     TEXT NOT NULL REFERENCES keys(name) ON DELETE CASCADE,
    secret_hash  BLOB NOT NULL,           -- SHA-256 of the full token; never the token
    created_at   INTEGER NOT NULL,
    valid_until  INTEGER                  -- set on the previous secret when the key is rotated
);

CREATE TABLE settings (
    name        TEXT PRIMARY KEY,
    value       TEXT NOT NULL,            -- JSON
    updated_at  INTEGER NOT NULL
);

CREATE TABLE revisions (
    revision    INTEGER PRIMARY KEY,
    actor       TEXT NOT NULL,
    summary     TEXT NOT NULL,
    created_at  INTEGER NOT NULL
);

CREATE TABLE usage (
    id                    TEXT PRIMARY KEY,   -- unique per provider call; makes the write idempotent
    request_id            TEXT NOT NULL,
    trace_id              TEXT,
    attempt               INTEGER NOT NULL DEFAULT 1,
    endpoint              TEXT NOT NULL,      -- 'chat' | 'messages' | 'embeddings' | 'rerank'
    key_name              TEXT NOT NULL,      -- a key name, or '_verify'
    model                 TEXT NOT NULL,      -- the alias requested; '' for '_verify'
    target                TEXT NOT NULL,      -- 'provider/upstream-model' this attempt was sent to
    billing               TEXT NOT NULL,      -- 'pay_per_use' | 'subscription' | 'none'
    response_model        TEXT,               -- as returned by the provider
    model_mismatch        INTEGER NOT NULL DEFAULT 0,
    status_code           INTEGER NOT NULL,
    error_type            TEXT,
    finish_reason         TEXT,
    stream                INTEGER NOT NULL DEFAULT 0,
    input_tokens          INTEGER NOT NULL DEFAULT 0,   -- total input, cache tokens included
    cache_read_tokens     INTEGER NOT NULL DEFAULT 0,   -- part of input_tokens
    cache_write_tokens    INTEGER NOT NULL DEFAULT 0,   -- part of input_tokens
    output_tokens         INTEGER NOT NULL DEFAULT 0,   -- total output, reasoning included
    reasoning_tokens      INTEGER,                      -- part of output_tokens; NULL when not reported
    tokens_source         TEXT NOT NULL,      -- 'provider' | 'estimated'
    cost_usd              REAL,               -- NULL when unknown or not applicable
    cost_source           TEXT NOT NULL,      -- 'provider' | 'price_table' | 'none'
    effort_requested      TEXT,
    effort_alias          TEXT,
    effort_sent           TEXT,               -- level | 'on' | 'budget:<N>' | 'none' | NULL
    effort_source         TEXT,               -- 'client' | 'alias_default' | 'alias_forced' | 'provider_default'
    effort_adjusted       INTEGER NOT NULL DEFAULT 0,
    max_output_requested  INTEGER,
    max_output_alias      INTEGER,
    max_output_sent       INTEGER,
    params_changed        TEXT,               -- 'name:action,name:action'
    content_empty         INTEGER NOT NULL DEFAULT 0,   -- no text and no tool call
    ttft_ms               INTEGER,
    started_at            INTEGER NOT NULL,
    completed_at          INTEGER NOT NULL,
    config_revision       INTEGER NOT NULL,
    proxy_version         TEXT NOT NULL
);
CREATE INDEX idx_usage_time       ON usage(completed_at);
CREATE INDEX idx_usage_key_time   ON usage(key_name, completed_at);
CREATE INDEX idx_usage_model_time ON usage(model, completed_at);

CREATE TABLE usage_daily (                -- updated in the same transaction as `usage`; source of budget counters
    day                 TEXT NOT NULL,    -- '2026-11-11', UTC
    key_name            TEXT NOT NULL,
    model               TEXT NOT NULL,
    target              TEXT NOT NULL,
    billing             TEXT NOT NULL,
    requests            INTEGER NOT NULL DEFAULT 0,
    errors              INTEGER NOT NULL DEFAULT 0,
    input_tokens        INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens   INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens       INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens    INTEGER NOT NULL DEFAULT 0,
    cost_usd            REAL NOT NULL DEFAULT 0,
    PRIMARY KEY (day, key_name, model, target)
);

CREATE TABLE budget_totals (              -- running totals of `never` budgets; not subject to retention
    scope_type  TEXT NOT NULL,            -- 'global' | 'key' | 'model'
    scope_name  TEXT NOT NULL,
    cost_usd    REAL NOT NULL DEFAULT 0,
    PRIMARY KEY (scope_type, scope_name)
);

CREATE TABLE audit (
    id              TEXT PRIMARY KEY,
    actor           TEXT NOT NULL,
    action          TEXT NOT NULL,
    resource_type   TEXT NOT NULL,
    resource_name   TEXT NOT NULL,
    previous_value  TEXT,
    new_value       TEXT,
    result          TEXT NOT NULL,
    revision        INTEGER,
    created_at      INTEGER NOT NULL
);
