ALTER TABLE engine.orders
    ADD COLUMN time_in_force text NOT NULL DEFAULT 'GTC' CHECK (time_in_force IN ('GTC', 'IOC'));
