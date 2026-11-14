-- Migration 002: usage accounting

ALTER TABLE budget_totals ADD COLUMN since_revision INTEGER NOT NULL DEFAULT 0;  -- revision the counter started at

DROP TABLE usage_daily;                   -- nothing wrote it before this migration
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
    reasoning_tokens    INTEGER NOT NULL DEFAULT 0,
    cost_usd            REAL NOT NULL DEFAULT 0,
    PRIMARY KEY (day, key_name, model, target, billing)   -- a provider whose billing changes keeps its rows apart
);
