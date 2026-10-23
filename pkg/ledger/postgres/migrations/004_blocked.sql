-- Admins can block an account from trading.
ALTER TABLE ledger.accounts ADD COLUMN blocked boolean NOT NULL DEFAULT false;
