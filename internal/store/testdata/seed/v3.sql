-- Rows valid at user_version 3 (migrations 001 to 003): one or more in
-- every table, with every foreign key pointing at a row that exists.
-- TestMigrations seeds them and requires that migrating to head keeps them
-- with their values.

INSERT INTO providers (name, spec, updated_at) VALUES
    ('alpha', '{"protocol":"openai","base_url":"http://127.0.0.1:9/v1","api_key_env":"ALPHA_KEY","billing":"pay_per_use"}', 1763164800),
    ('beta', '{"protocol":"anthropic","base_url":"http://127.0.0.1:9","api_key_env":"BETA_KEY","billing":"subscription"}', 1763164801);

INSERT INTO targets (id, provider, upstream_model, spec, verified_at, verified_version, updated_at) VALUES
    ('alpha/m-large', 'alpha', 'm-large', '{"kind":"chat","pricing":{"input":3,"output":15}}', 1763164900, 'v0.1.0', 1763164802),
    ('beta/m-small', 'beta', 'm-small', '{"kind":"chat"}', NULL, NULL, 1763164803);

INSERT INTO models (name, spec, updated_at) VALUES
    ('fast', '{"reasoning":"low","max_output_tokens":1024}', 1763164804),
    ('deep', '{}', 1763164805);

INSERT INTO model_targets (model, position, target) VALUES
    ('fast', 0, 'beta/m-small'),
    ('fast', 1, 'alpha/m-large'),
    ('deep', 0, 'alpha/m-large');

INSERT INTO keys (name, spec, created_at, revoked_at) VALUES
    ('app', '{"budget":{"usd":10,"period":"month"}}', 1763164806, NULL),
    ('old', '{}', 1763164807, 1763164999);

INSERT INTO key_models (key_name, model) VALUES
    ('app', 'fast'),
    ('app', 'deep'),
    ('old', '*');

INSERT INTO key_secrets (public_id, key_name, secret_hash, created_at, valid_until) VALUES
    ('pk1', 'app', X'00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff', 1763164806, 1763168406),
    ('pk2', 'app', X'ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100', 1763164900, NULL),
    ('pk3', 'old', X'0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f', 1763164807, NULL);

INSERT INTO settings (name, value, updated_at) VALUES
    ('retention', '{"usage_days":90}', 1763164808),
    ('log_level', '"info"', 1763164809);

INSERT INTO revisions (revision, actor, summary, created_at) VALUES
    (1, 'cli', 'apply config.yaml', 1763164810),
    (2, 'admin', 'revoke key old', 1763164999);

INSERT INTO usage (id, request_id, trace_id, attempt, endpoint, key_name, model, target, billing,
    response_model, model_mismatch, status_code, error_type, finish_reason, stream,
    input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, reasoning_tokens,
    tokens_source, cost_usd, cost_source, effort_requested, effort_alias, effort_sent,
    effort_source, effort_adjusted, max_output_requested, max_output_alias, max_output_sent,
    params_changed, content_empty, ttft_ms, started_at, completed_at, config_revision, proxy_version) VALUES
    ('u1', 'r1', '4bf92f3577b34da6a3ce929d0e0e4736', 1, 'chat', 'app', 'fast', 'beta/m-small', 'subscription',
     'm-small-2026', 0, 200, NULL, 'stop', 1,
     1200, 1000, 0, 300, 120,
     'provider', NULL, 'none', 'high', 'low', 'low',
     'client', 1, 4096, 1024, 1024,
     'temperature:dropped', 0, 180, 1763165000000, 1763165002000, 2, 'v0.1.0'),
    ('u2', 'r2', NULL, 2, 'chat', 'app', 'fast', 'alpha/m-large', 'pay_per_use',
     NULL, 0, 502, 'upstream_error', NULL, 0,
     0, 0, 0, 0, NULL,
     'estimated', 0.0125, 'price_table', NULL, NULL, NULL,
     NULL, 0, NULL, NULL, NULL,
     NULL, 1, NULL, 1763165003000, 1763165004000, 2, 'v0.1.0');

-- Two rows that differ only in billing, which migration 002 put in the key;
-- since migration 003 a figure no record reported is NULL, a reported 0 is 0.
INSERT INTO usage_daily (day, key_name, model, target, billing, requests, errors,
    input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, reasoning_tokens, cost_usd) VALUES
    ('2026-11-15', 'app', 'fast', 'beta/m-small', 'subscription', 1, 0, 1200, 1000, 0, 300, 0, NULL),
    ('2026-11-15', 'app', 'fast', 'beta/m-small', 'pay_per_use', 1, 1, 0, 0, 0, 0, NULL, 0.0125);

INSERT INTO budget_totals (scope_type, scope_name, cost_usd, since_revision) VALUES
    ('global', '', 4.75, 0),
    ('key', 'app', 2.5, 1),
    ('model', 'deep', 0, 2);

INSERT INTO audit (id, actor, action, resource_type, resource_name, previous_value, new_value, result, revision, created_at) VALUES
    ('a1', 'cli', 'apply', 'config', 'config.yaml', NULL, '{"models":2}', 'ok', 1, 1763164810),
    ('a2', 'admin', 'revoke', 'key', 'old', '{"revoked_at":null}', '{"revoked_at":1763164999}', 'ok', 2, 1763164999),
    ('a3', 'admin', 'delete', 'model', 'missing', NULL, NULL, 'not_found', NULL, 1763165100);
