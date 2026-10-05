-- One long-lived API token per user, for bots. Only its digest is stored.
CREATE TABLE gateway.api_tokens (
    user_id      text PRIMARY KEY REFERENCES gateway.users (id) ON DELETE CASCADE,
    token_digest bytea NOT NULL UNIQUE,
    hint         text NOT NULL,
    created_at   timestamptz NOT NULL
);

CREATE INDEX sessions_user ON gateway.sessions (user_id);
