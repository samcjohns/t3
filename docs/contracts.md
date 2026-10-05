# Market Engine Contracts

Canonical JSON payloads for the market engine. These are defined in `pkg/engine/types.go`.

**Rule: no breaking changes to contract fields without a version bump.** Renaming, removing, or changing the type or meaning of a field is breaking. Adding an optional field is not.

## Units

- Prices are integers in minor currency units (cents). `1005` = $10.05.
- Quantities are whole shares and always positive.

## Order

Submitted by a trusted caller (the API gateway). The engine does no authentication.

```json
{
  "id": "ord-7f3a",
  "account_id": "acct-42",
  "symbol": "ACME",
  "direction": "BUY",
  "type": "LIMIT",
  "quantity": 100,
  "limit_price": 1010,
  "max_cost": 0,
  "time_in_force": "GTC",
  "sequence": 17
}
```

| Field | Rules |
| --- | --- |
| `id` | Required. Must never be reused; the engine's journal rejects any ID it has seen before. The gateway generates random IDs. |
| `account_id` | Required. |
| `symbol` | Required. Each symbol has its own book and auction. |
| `direction` | `BUY` or `SELL`. |
| `type` | `LIMIT` or `MARKET`. |
| `quantity` | Must be greater than 0. |
| `limit_price` | Must be greater than 0 for `LIMIT` and exactly 0 for `MARKET`. |
| `max_cost` | Must be greater than 0 for `MARKET` `BUY`, and exactly 0 for every other order. This is the most the order may spend: at clearing price `p` it fills at most `floor(max_cost / p)` shares. |
| `time_in_force` | `GTC` (good till cancelled; the default when empty or omitted) or `IOC` (immediate or cancel). It only affects limit orders. |
| `sequence` | Set by the engine when it accepts the order, and used as the time-priority tie-break. Any value sent in is ignored. |

Lifecycle:

- If a `GTC` limit order isn't fully filled, the rest of it stays in the book and joins the next tick's batch.
- If an `IOC` limit order isn't fully filled, the rest of it expires at the end of the tick it entered, like a market order.
- If a `MARKET` order isn't fully filled, the rest of it expires at the end of the tick it entered.

## Execution

```json
{
  "symbol": "ACME",
  "buy_order_id": "ord-7f3a",
  "sell_order_id": "ord-91c0",
  "buy_account_id": "acct-42",
  "sell_account_id": "acct-7",
  "price": 1005,
  "quantity": 100
}
```

Every execution in a book's tick has the same `price`, which is that book's clearing price.

## TickResult

The engine publishes one of these every heartbeat (10 s by default).

```json
{
  "tick": 128,
  "timestamp": "2026-10-04T12:00:10Z",
  "books": [
    {
      "symbol": "ACME",
      "clearing_price": 1005,
      "volume": 100,
      "executions": [ /* Execution */ ],
      "expired_order_ids": []
    }
  ]
}
```

- `tick` increases by exactly 1 per heartbeat, starting at 1.
- `books` has one entry for each symbol that had orders (new or resting) in the batch, sorted by symbol. It is `[]` when there are none.
- `clearing_price` and `volume` are both `0` when the book did not cross.
- `executions` and `expired_order_ids` are always arrays, never `null`.

## Clearing rules (FBA)

For each symbol, on each tick:

1. The candidate prices are the batch's distinct limit prices. Market orders trade at any price, but they never set one.
2. The engine picks the candidate that **maximises executed volume**. If several tie, it picks the one with the **smallest buy/sell imbalance**. If they still tie, it takes the **midpoint, rounded down**, of the tied range.
3. If the batch has no limit prices, or no price crosses, nothing executes.
4. Every fill trades at the single clearing price.
5. A market buy's demand at price `p` is capped by `floor(max_cost / p)`.
6. The larger side is filled in priority order. Market orders come first, then the best limit price, then the lowest `sequence`.

## Ledger funding

Every order must be reserved with the ledger before it is submitted to the engine. If the engine rejects the order, its hold is released. The hold is sized so that settlement can never overdraw an account:

| Order | Hold |
| --- | --- |
| Limit buy | `quantity × limit_price` in cash |
| Market buy | `max_cost` in cash |
| Any sell | `quantity` shares |

The ledger applies each `TickResult` exactly once, in `tick` order. A limit buy that fills below its limit gets the difference back. When an order is fully filled, or listed in `expired_order_ids`, whatever is left of its hold is released.
