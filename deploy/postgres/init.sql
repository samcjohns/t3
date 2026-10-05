-- Runs once, when the Postgres data volume is first created. Each service gets
-- its own role that owns only its own schema (docs/persistence.md).
-- These passwords are for local development only.
CREATE ROLE t3_engine LOGIN PASSWORD 'engine-dev';
CREATE ROLE t3_ledger LOGIN PASSWORD 'ledger-dev';
CREATE ROLE t3_gateway LOGIN PASSWORD 'gateway-dev';

CREATE SCHEMA engine AUTHORIZATION t3_engine;
CREATE SCHEMA ledger AUTHORIZATION t3_ledger;
CREATE SCHEMA gateway AUTHORIZATION t3_gateway;

REVOKE CREATE ON SCHEMA public FROM PUBLIC;
