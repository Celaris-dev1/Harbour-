CREATE TABLE IF NOT EXISTS goals (
  id            TEXT PRIMARY KEY,
  name          TEXT NOT NULL UNIQUE,
  agent         TEXT NOT NULL,
  input         JSONB NOT NULL DEFAULT '{}',
  state         TEXT NOT NULL,
  paused_from   TEXT,
  parent_id     TEXT REFERENCES goals(id),
  created_by    TEXT NOT NULL,
  cursor        INT NOT NULL DEFAULT 0,
  lease_owner   TEXT,
  lease_epoch   BIGINT NOT NULL DEFAULT 0,
  lease_expires TIMESTAMPTZ,
  reason        TEXT NOT NULL DEFAULT '',
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS goals_claim ON goals(state, lease_expires);

CREATE TABLE IF NOT EXISTS effects (
  id          BIGSERIAL PRIMARY KEY,
  idem_key    TEXT NOT NULL UNIQUE,
  goal_id     TEXT NOT NULL REFERENCES goals(id),
  step        INT NOT NULL,
  tool        TEXT NOT NULL,
  args        JSONB NOT NULL,
  status      TEXT NOT NULL CHECK (status IN ('intent','committed','failed','needs_review')),
  result      JSONB,
  error       TEXT NOT NULL DEFAULT '',
  attempts    INT NOT NULL DEFAULT 0,
  lease_epoch BIGINT NOT NULL,
  worker      TEXT NOT NULL,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (goal_id, step)
);

CREATE TABLE IF NOT EXISTS steps (
  goal_id      TEXT NOT NULL REFERENCES goals(id),
  step         INT NOT NULL,
  tool         TEXT NOT NULL,
  args         JSONB NOT NULL,
  result       JSONB,
  idem_key     TEXT NOT NULL,
  committed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (goal_id, step)
);

CREATE TABLE IF NOT EXISTS messages (
  id         BIGSERIAL PRIMARY KEY,
  goal_id    TEXT NOT NULL REFERENCES goals(id),
  channel    TEXT NOT NULL,
  source     TEXT NOT NULL CHECK (source IN ('operator','peer_agent','scheduler','tool_result','retrieved_document')),
  sender     TEXT NOT NULL DEFAULT '',
  trusted    BOOLEAN NOT NULL,
  body       JSONB NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS messages_goal ON messages(goal_id, id);

CREATE TABLE IF NOT EXISTS events (
  seq        BIGSERIAL PRIMARY KEY,
  goal_id    TEXT NOT NULL,
  kind       TEXT NOT NULL,
  data       JSONB NOT NULL DEFAULT '{}',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS events_goal ON events(goal_id, seq);
