# Decision: Persistent Storage

- **Status:** Accepted, implemented
- **Date:** 2026-10-04

## Context

Today every container keeps its state in memory, so a restart loses users, balances, holds and resting orders. Persistence has to survive crashes without the engine and the ledger falling out of agreement. Two failures matter most:

1. **Settlement gap.** The engine clears a tick and then crashes before the ledger applies it. Those fills are lost. If the engine later re-sends the tick, they risk being applied twice.
2. **Orphaned hold.** The gateway reserves funds with the ledger, then crashes before the order reaches the engine. The cash stays held with no order behind it.

We considered making the ledger the store for the whole system, and rejected it (see [Alternatives](#alternatives-considered)).

## Decision

**Each container owns its own state, kept in its own schema on a single PostgreSQL server. The engine's `tick_results` table is the durable record of what happened in the market. The ledger, reporting service and future consumers recover by catching up from it.**

### Rules

1. **Each container has its own schema.** The schemas are `engine`, `ledger`, `gateway` and later `reporting`. Each container connects with its own database role, which has privileges on its own schema only. No container reads another's tables; data crosses boundaries only through service ports.
2. **Only adapters touch the database.** The domain packages stay free of dependencies. Persistence sits behind storage interfaces, with Postgres implementations in adapter packages (`pkg/<service>/postgres`). The current in-memory code becomes the in-memory implementation, used by unit tests.
3. **Durable before acknowledged.** The engine doesn't return `202 Accepted` until the order is committed. It doesn't call the tick callback until the tick's result is committed.
4. **Exactly-once settlement.** The ledger applies each tick in one transaction that also advances `last_tick`. It recovers by fetching every tick after `last_tick` from the engine. `ApplyTick` already enforces strict ordering and rejects replays.
5. **Reporting is a cache.** It stores nothing at first and rebuilds by replaying `tick_results` on startup. It gets its own schema only once replaying becomes too slow, and even then that data stays derived, never the source of truth.

### Schemas

These are sketches; the migrations will be the definitive versions.

**`engine`**

```sql
orders (
  id text primary key, account_id text, symbol text,
  direction text, type text, quantity bigint,
  limit_price bigint, max_cost bigint,
  sequence bigint unique not null,
  remaining bigint not null,
  status text not null check (status in ('pending','resting','filled','expired')),
  accepted_at timestamptz not null, closed_tick bigint
)
tick_results (tick bigint primary key, timestamp timestamptz not null, result jsonb not null)
```

The sequence and tick counters are recovered as `max(sequence)` and `max(tick)`.

**`ledger`**

```sql
accounts (id text primary key, cash bigint not null, cash_held bigint not null,
          check (cash_held between 0 and cash))
holdings (account_id text, symbol text, quantity bigint not null, held bigint not null,
          primary key (account_id, symbol), check (held between 0 and quantity))
holds    (order_id text primary key, account_id text, symbol text, direction text,
          type text, limit_price bigint, remaining bigint, cash bigint, shares bigint,
          created_at timestamptz not null)
entries  (id bigserial primary key, account_id text not null, kind text not null,
          tick bigint, order_id text, cash_delta bigint not null,
          symbol text, share_delta bigint not null, created_at timestamptz not null)
state    (last_tick bigint not null)  -- exactly one row
```

`entries` is an append-only audit log of every cash and share movement. `accounts` and `holdings` are running totals updated in the same transaction. Replaying `entries` must reproduce them, which gives us an audit check.

**`gateway`**

```sql
users    (id text primary key, username text unique not null, role text not null,
          salt bytea not null, hash bytea not null, created_at timestamptz not null)
sessions (token_digest bytea primary key, user_id text not null, expires_at timestamptz not null)
```

### Write paths

- **Order submission.** The engine assigns the sequence and inserts the order as `pending` under its intake lock. After the commit, it adds the order to the in-memory buffer and acknowledges it. Holding the lock during the insert serialises submissions, which is acceptable at a 10-second batch cadence.
- **Tick.** The engine matches into a copy of its book state. In one transaction, it inserts `tick_results` and updates every affected order's `remaining` and `status`. Only after the commit does it swap in the new in-memory state and call the tick callback. If the commit fails, the tick isn't published and the next heartbeat retries the same batch.
- **Settlement.** The ledger's apply-tick transaction does five things: checks the holds, updates `accounts`, `holdings` and `holds`, appends to `entries`, and sets `state.last_tick`. Any check failure rolls back the whole transaction.

### Recovery

| Container | On startup |
| --- | --- |
| Engine | Load orders with status `pending` or `resting` into the books, in sequence order, then restore the sequence and tick counters. |
| Ledger | Load the account state, then catch up by fetching ticks after `last_tick` from the engine. Then reconcile holds (see below). |
| Gateway | Nothing to do. Users and sessions are read on demand. |
| Reporting | Replay all `tick_results`. |

**Hold reconciliation.** A hold uses the same ID as its order. Any hold older than a grace period (30 s) whose order the engine doesn't know about is an orphan of failure 2, and gets released. The ledger runs this check on startup and periodically. The engine port needs a call that reports which of a given set of order IDs it knows.

**Registration** writes to two services: the gateway's user record and the ledger account. Opening the ledger account and crediting the starting cash must both be safe to retry. The starting-cash entry is unique per account, so a crash in between is repaired on the user's next login instead of leaving a user without a usable account.

## Consequences

- **Postgres becomes required to run the system.** Tests that need it use a real Postgres, selected by `T3_TEST_DATABASE_URL` and run via Docker. Unit tests keep using the in-memory implementations.
- **The project gets its first external dependency:** the Postgres driver `pgx`, used by the adapter packages only.
- **The engine adds a commit to every order and every tick.** At a 10 s heartbeat this is negligible: about 8,640 tick rows a day, plus one row per order.
- **The ledger needs a way to read ticks from the engine.** That's a "ticks after N" call on an engine-side port. In-process it reads the engine's store; once the containers are split, it becomes a network call or stream.
- **Ledger lag needs a policy.** If the ledger stops applying ticks (for example on `ErrInconsistentTick`), trading continues against holds the ledger can no longer settle. The gateway should stop accepting orders once the ledger falls more than a few ticks behind, and operators should be alerted.
- **Backups:** we back up a single Postgres instance with point-in-time recovery. Restoring it brings every schema back to the same moment, so they stay consistent with each other.

## Alternatives considered

- **Use the ledger as the store for everything.** Rejected. The ledger is the right owner for financial records, but putting the order book, users and market history there would make every container depend on it. It also doesn't solve either failure, which is about agreement between services, not about where the rows live.
- **One transaction across the engine and ledger tables.** Both are on the same server, and this would remove both failures outright. Rejected because it permanently fuses the two services, defeating the container split in `architecture.md`.
- **Pure event sourcing**, where only the engine's inputs are logged and all state is rebuilt by replay. Rejected for now: state tables are simpler to query and operate. Because the matcher always gives the same output for the same input, replaying the stored history can still check the stored state.
- **SQLite per container.** Simple and embedded, but it needs a volume per container and loses the shared backup and point-in-time recovery story.
- **Kafka or NATS JetStream for the tick log.** This is the expected next step once the containers run as separate processes and market makers need a live stream. Until then, a Postgres table is enough.
