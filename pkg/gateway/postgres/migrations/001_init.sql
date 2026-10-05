CREATE TABLE gateway.users (
    id         text PRIMARY KEY,
    username   text NOT NULL UNIQUE,
    role       text NOT NULL CHECK (role IN ('trader', 'admin')),
    salt       bytea NOT NULL,
    hash       bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE gateway.sessions (
    token_digest bytea PRIMARY KEY,
    user_id      text NOT NULL REFERENCES gateway.users (id) ON DELETE CASCADE,
    expires_at   timestamptz NOT NULL
);

CREATE INDEX sessions_expiry ON gateway.sessions (expires_at);
