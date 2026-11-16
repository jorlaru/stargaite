-- Migration 003: a figure no record reported is NULL in usage_daily

ALTER TABLE usage_daily RENAME TO usage_daily_002;
CREATE TABLE usage_daily (
    day                 TEXT NOT NULL,
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
    reasoning_tokens    INTEGER,          -- NULL when no record of the row reported it
    cost_usd            REAL,             -- NULL when no record of the row reported it
    PRIMARY KEY (day, key_name, model, target, billing)   -- a provider whose billing changes keeps its rows apart
);
INSERT INTO usage_daily SELECT day, key_name, model, target, billing, requests, errors, input_tokens, cache_read_tokens,
    cache_write_tokens, output_tokens, NULLIF(reasoning_tokens, 0), NULLIF(cost_usd, 0)
    FROM usage_daily_002;             -- an old 0 cannot be told from an unknown, so it becomes unknown
DROP TABLE usage_daily_002;
