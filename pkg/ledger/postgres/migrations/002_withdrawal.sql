-- Admins can remove cash from an account.
ALTER TABLE ledger.entries DROP CONSTRAINT entries_kind_check;
ALTER TABLE ledger.entries ADD CONSTRAINT entries_kind_check
    CHECK (kind IN ('deposit', 'withdrawal', 'share_deposit', 'buy', 'sell'));
