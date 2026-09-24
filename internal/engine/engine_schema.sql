-- Self-contained schema for the Postgres Engine, deliberately separate from
-- internal/store's schema (this package is a parallel governance-layer
-- demonstration, not a replacement for the production runtime's tables).
CREATE TABLE IF NOT EXISTS engine_steps (
  key        TEXT PRIMARY KEY,
  status     TEXT NOT NULL CHECK (status IN ('intent','committed','failed','needs_review')),
  result     JSONB,
  error      TEXT NOT NULL DEFAULT '',
  attempts   INT NOT NULL DEFAULT 0,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS engine_timers (
  key      TEXT PRIMARY KEY,
  deadline TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS engine_signals (
  id         BIGSERIAL PRIMARY KEY,
  key        TEXT NOT NULL,
  name       TEXT NOT NULL,
  payload    JSONB NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS engine_signals_lookup ON engine_signals(key, name, id);
