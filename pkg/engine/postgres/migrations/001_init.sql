CREATE TABLE engine.orders (
    id          text PRIMARY KEY,
    account_id  text NOT NULL,
    symbol      text NOT NULL,
    direction   text NOT NULL CHECK (direction IN ('BUY', 'SELL')),
    type        text NOT NULL CHECK (type IN ('LIMIT', 'MARKET')),
    quantity    bigint NOT NULL CHECK (quantity > 0),
    limit_price bigint NOT NULL,
    max_cost    bigint NOT NULL,
    sequence    bigint NOT NULL UNIQUE,
    remaining   bigint NOT NULL CHECK (remaining BETWEEN 0 AND quantity),
    status      text NOT NULL CHECK (status IN ('pending', 'resting', 'filled', 'expired')),
    accepted_at timestamptz NOT NULL DEFAULT now(),
    closed_tick bigint
);

CREATE INDEX orders_open ON engine.orders (sequence) WHERE status IN ('pending', 'resting');

CREATE TABLE engine.tick_results (
    tick      bigint PRIMARY KEY,
    timestamp timestamptz NOT NULL,
    result    jsonb NOT NULL
);
