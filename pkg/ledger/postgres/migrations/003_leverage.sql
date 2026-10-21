-- Leverage: the lending pool, loans, liquidation and bankruptcy.
ALTER TABLE ledger.accounts
    ADD COLUMN status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'liquidating', 'bankrupt')),
    ADD COLUMN pool_shares bigint NOT NULL DEFAULT 0 CHECK (pool_shares >= 0),
    -- Scaled debt per leverage ratio, as {"<ratio>": <integer>}.
    ADD COLUMN loans jsonb NOT NULL DEFAULT '{}';

ALTER TABLE ledger.holds
    ADD COLUMN leverage bigint NOT NULL DEFAULT 0 CHECK (leverage >= 0),
    ADD COLUMN loan bigint NOT NULL DEFAULT 0 CHECK (loan >= 0);

ALTER TABLE ledger.entries DROP CONSTRAINT entries_kind_check;
ALTER TABLE ledger.entries ADD CONSTRAINT entries_kind_check
    CHECK (kind IN ('deposit', 'withdrawal', 'share_deposit', 'buy', 'sell', 'borrow', 'repay', 'lend', 'redeem'));

ALTER TABLE ledger.state
    ADD COLUMN pool_cash bigint NOT NULL DEFAULT 0,
    ADD COLUMN pool_reserved bigint NOT NULL DEFAULT 0,
    ADD COLUMN pool_shares bigint NOT NULL DEFAULT 0,
    -- Borrow index and total scaled debt per leverage ratio.
    ADD COLUMN pool_index jsonb NOT NULL DEFAULT '{}',
    ADD COLUMN pool_scaled jsonb NOT NULL DEFAULT '{}',
    ADD CHECK (pool_reserved BETWEEN 0 AND pool_cash AND pool_shares >= 0);

-- Last clearing prices, which value holdings for margin checks.
CREATE TABLE ledger.marks (
    symbol text PRIMARY KEY,
    price  bigint NOT NULL CHECK (price > 0)
);

CREATE TABLE ledger.bankruptcies (
    account_id  text PRIMARY KEY REFERENCES ledger.accounts (id),
    tick        bigint NOT NULL,
    time        timestamptz NOT NULL,
    written_off bigint NOT NULL CHECK (written_off > 0)
);
