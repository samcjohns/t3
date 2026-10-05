ALTER TABLE gateway.users DROP CONSTRAINT users_role_check;
ALTER TABLE gateway.users ADD CONSTRAINT users_role_check CHECK (role IN ('trader', 'admin', 'market_maker'));
