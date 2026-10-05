# Public API (v1)

The API gateway is the only public entry point. All bodies are JSON. Money is in integer cents, and the domain payloads are described in [contracts.md](contracts.md).

## Conventions

- **Auth:** send `Authorization: Bearer <token>`, using a token from `POST /v1/auth/login`. Tokens are opaque and expire after 24 hours.
- **Errors:** every error returns `{"error": {"code": "...", "message": "..."}}`.
- **Rate limits:** clients get 10 requests per second with bursts of up to 20. Limits are counted per user when a valid token is sent, and per client IP otherwise. An over-limit request gets `429` with a `Retry-After` header.
- **Request bodies:** a body must be a single JSON object of at most 64 KiB. Unknown fields are rejected.
- **Paging:** `limit` query parameters accept 1–1000 and default to 100.

## Endpoints

| Method & path | Auth | Description |
| --- | --- | --- |
| `POST /v1/auth/register` | — | `{username, password}` → `201` user. Creates a ledger account with the starting cash. |
| `POST /v1/auth/login` | — | `{username, password}` → `{token, expires_at, user}` |
| `POST /v1/auth/logout` | trader | Revokes the token → `204` |
| `GET /v1/account` | trader | Ledger balances and holdings, including the amounts held. |
| `GET /v1/account/portfolio` | trader | Holdings valued at last prices. |
| `GET /v1/account/trades?limit=` | trader | Your own fills, newest first. |
| `POST /v1/orders` | trader | Places an order, returning `202` with the accepted order. See below. |
| `GET /v1/market/symbols` | — | The tradable symbols. |
| `GET /v1/market/{symbol}/quote` | — | Last clearing price. Returns `404` if the symbol hasn't traded yet. |
| `GET /v1/market/{symbol}/trades?limit=` | — | The public tape, newest first, with no account or order IDs. |
| `GET /v1/market/{symbol}/candles?interval=&limit=` | — | OHLCV candles, oldest first. `interval` is one of `1m`, `5m`, `15m`, `1h` or `1d`. |
| `POST /v1/admin/accounts/{id}/cash` | admin | `{amount}`: credits cash. |
| `POST /v1/admin/accounts/{id}/shares` | admin | `{symbol, quantity}`: credits shares. |
| `GET /healthz` | — | Liveness check. Not rate limited. |
| `GET /metrics` | — | Prometheus counters. Keep this on an internal network. |

## Placing an order

```json
{"symbol": "ACME", "direction": "BUY", "type": "LIMIT", "quantity": 10, "limit_price": 1010}
```

`max_cost` is required on market buys. A client can't set `id`, `account_id` or `sequence`; the gateway assigns the ID, the account comes from the token, and the engine assigns the sequence.

The gateway reserves the order's funding with the ledger before the engine sees the order. If the engine rejects it, the hold is released. The order fills at the next heartbeat.

## Error codes

| Status | Codes |
| --- | --- |
| 400 | `bad_request`, `invalid_order`, `invalid_amount`, `invalid_interval`, `invalid_username`, `invalid_password` |
| 401 | `unauthorized`, `invalid_credentials` |
| 403 | `forbidden` |
| 404 | `unknown_symbol`, `unknown_account` |
| 409 | `username_taken`, `duplicate_order` |
| 422 | `insufficient_funds`, `insufficient_shares` |
| 429 | `rate_limited` |
| 500 | `internal_error`. Details are logged, never returned. |

## Running

```sh
T3_ADMIN_PASSWORD=change-me go run ./cmd/server
```

| Variable | Default | |
| --- | --- | --- |
| `T3_ADDR` | `:8080` | Listen address |
| `T3_HEARTBEAT` | `10s` | Batch interval |
| `T3_SYMBOLS` | `ACME` | Comma-separated tradable symbols |
| `T3_STARTING_CASH` | `1000000` | Cents credited to each new trader |
| `T3_ALLOWED_ORIGINS` | — | Comma-separated CORS origins |
| `T3_ADMIN_USERNAME` | `admin` | Bootstrap admin username |
| `T3_ADMIN_PASSWORD` | — | If unset, no admin is created |
| `T3_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
