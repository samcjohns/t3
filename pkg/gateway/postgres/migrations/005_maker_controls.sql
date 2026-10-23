-- Admins' controls over each market maker: whether it is paused, and the
-- settings that override its own defaults.
CREATE TABLE gateway.maker_controls (
    user_id   text PRIMARY KEY REFERENCES gateway.users (id) ON DELETE CASCADE,
    paused    boolean NOT NULL,
    overrides jsonb NOT NULL
);
