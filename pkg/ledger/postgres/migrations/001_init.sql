CREATE TABLE ledger.accounts (
    id        text PRIMARY KEY,
    cash      bigint NOT NULL,
    cash_held bigint NOT NULL,
    CHECK (cash_held BETWEEN 0 AND cash)
);

CREATE TABLE ledger.holdings (
    account_id text NOT NULL REFERENCES ledger.accounts (id),
    symbol     text NOT NULL,
    quantity   bigint NOT NULL,
    held       bigint NOT NULL,
    PRIMARY KEY (account_id, symbol),
    CHECK (quantity > 0 AND held BETWEEN 0 AND quantity)
);

CREATE TABLE ledger.holds (
    order_id   text PRIMARY KEY,
    account_id text NOT NULL REFERENCES ledger.accounts (id),
    "order"    jsonb NOT NULL,
    remaining  bigint NOT NULL CHECK (remaining > 0),
    cash       bigint NOT NULL CHECK (cash >= 0),
    shares     bigint NOT NULL CHECK (shares >= 0),
    created_at timestamptz NOT NULL
);

-- Append-only: every movement of cash or shares.
CREATE TABLE ledger.entries (
    id          bigserial PRIMARY KEY,
    account_id  text NOT NULL REFERENCES ledger.accounts (id),
    kind        text NOT NULL CHECK (kind IN ('deposit', 'share_deposit', 'buy', 'sell')),
    tick        bigint,
    order_id    text,
    reference   text,
    cash_delta  bigint NOT NULL,
    symbol      text,
    share_delta bigint NOT NULL,
    created_at  timestamptz NOT NULL
);

CREATE UNIQUE INDEX entries_reference ON ledger.entries (account_id, reference) WHERE reference IS NOT NULL;
CREATE INDEX entries_account ON ledger.entries (account_id, id);

CREATE TABLE ledger.state (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    last_tick bigint NOT NULL
);

INSERT INTO ledger.state (last_tick) VALUES (0);
