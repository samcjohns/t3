# Public API (v1)

The API gateway is the only public entry point. All bodies are JSON. Money is in integer cents, and the domain payloads are described in [contracts.md](contracts.md).

To trade from your own code, see [bots.md](bots.md).

## Conventions

- **Auth:** send `Authorization: Bearer <token>`. There are two kinds of token, and both are opaque:
  - A **session token** comes from `POST /v1/auth/login` and expires after 24 hours. The web app uses these.
  - An **API token** is for bots and scripts. It starts with `t3_`, never expires, and stays valid until it is regenerated or revoked. Create one from the account popup in the web app (click your username), or with `POST /v1/account/api-token`. Each account has at most one. It is shown only when created, because the server stores just its SHA-256 digest.

  API tokens work on every `trader` endpoint. Endpoints marked `session` need a session token, so a leaked API token can trade but can't change the password, manage tokens or lock the owner out.
- **Errors:** every error returns `{"error": {"code": "...", "message": "..."}}`.
- **Rate limits:** clients get 10 requests per second with bursts of up to 20. Limits are counted per user when a valid token is sent, and per client IP otherwise; `/v1/market/prices` is always counted per IP. Accounts with the internal `market_maker` role are exempt. An over-limit request gets `429` with a `Retry-After` header.
- **Request bodies:** a body must be a single JSON object of at most 64 KiB. Unknown fields are rejected.
- **Paging:** `limit` query parameters accept 1–1000 and default to 100.

## Endpoints

| Method & path | Auth | Description |
| --- | --- | --- |
| `POST /v1/auth/register` | — | `{username, password}` → `201` user. Creates a ledger account with the starting cash. |
| `POST /v1/auth/login` | — | `{username, password}` → `{token, expires_at, user}` |
| `POST /v1/auth/logout` | session | Revokes the session token → `204` |
| `GET /v1/account` | trader | Ledger balances and holdings, including the amounts held. |
| `GET /v1/account/profile` | trader | `{user, api_token}`: the user with `created_at`, and the API token's `{hint, created_at}`, or `null` if there is none. It never includes the token itself. |
| `POST /v1/account/password` | session | `{current_password, new_password}` → `204`. Signs out every other session. The API token keeps working. A wrong current password returns `403 invalid_credentials`, not `401`, because the session is still valid. |
| `POST /v1/account/api-token` | session | Creates an API token, replacing any existing one → `201 {token, hint, created_at}`. This is the only time `token` is returned. |
| `DELETE /v1/account/api-token` | session | Revokes the API token → `204` |
| `GET /v1/account/portfolio` | trader | Holdings valued at last prices. Each position's `cost_basis` is what its shares cost at their average purchase price, or `null` if some predate the retained trade history (such as deposited shares). |
| `GET /v1/account/trades?limit=` | trader | Your own fills, newest first. |
| `GET /v1/account/history?interval=&limit=` | trader | Your account's total value (cash plus holdings at last prices) as `open`/`high`/`low`/`close` candles, oldest first, with the same `interval` options as market candles. See below. |
| `POST /v1/orders` | trader | Places an order, returning `202` with the accepted order. See below. |
| `GET /v1/leaderboard?limit=` | — | Traders ranked by account value, highest first, as `{tick, starting_cash, traders, standings, you}`. Each standing has `rank`, `username`, `total_value` and `gain` (value less starting cash), and equal values share a rank. `you` is the caller's own standing when a trader token is sent, even outside the limit, and `null` otherwise. Admins and market makers are not ranked. Standings are computed once per tick, so a new trader appears after the next auction. |
| `GET /v1/market/symbols` | — | The tradable symbols, each with `name` and `reference_price`. |
| `GET /v1/market/prices` | — | Every symbol's current price, as one pre-built snapshot that changes once per tick. It supports `ETag`/`If-None-Match`, which returns `304` while unchanged. |
| `GET /v1/market/{symbol}/quote` | — | Last clearing price. Returns `404` if the symbol hasn't traded yet. |
| `GET /v1/market/{symbol}/trades?limit=` | — | The public tape, newest first, with no account or order IDs. |
| `GET /v1/market/{symbol}/candles?interval=&limit=` | — | OHLCV candles, oldest first. `interval` is one of `1m`, `5m`, `15m`, `1h` or `1d`. |
| `POST /v1/admin/accounts/{id}/cash` | admin | `{amount}`: credits cash. |
| `POST /v1/admin/accounts/{id}/shares` | admin | `{symbol, quantity}`: credits shares. |
| `GET /healthz` | — | Liveness check. Not rate limited. |
| `GET /readyz` | — | Returns `503 market_halted` while the ledger is too far behind to settle trades. |
| `GET /metrics` | — | Prometheus counters. Keep this on an internal network. |

## Placing an order

```json
{"symbol": "ACME", "direction": "BUY", "type": "LIMIT", "quantity": 10, "limit_price": 1010}
```

`max_cost` is required on market buys. `time_in_force` is `GTC` (the default) or `IOC`; any unfilled part of an `IOC` order expires after one auction. A client can't set `id`, `account_id` or `sequence`; the gateway assigns the ID, the account comes from the token, and the engine assigns the sequence.

The gateway reserves the order's funding with the ledger before the engine sees the order. If the engine rejects it, the hold is released. The order fills at the next heartbeat.

Order entry returns `503 market_halted` if the ledger falls more than 3 ticks behind the engine, because fills could no longer be settled. Market data stays available.

## Account history

History is rebuilt by rewinding the account's current balances through its fills, so it starts at the oldest retained fill and ends with the period of the latest tick. Every period in between appears, flat if nothing changed. Values are sampled once a minute at each symbol's closing price, so `high` and `low` are accurate to that resolution. Deposits are not fills, so they appear to have been there from the start.

## Error codes

| Status | Codes |
| --- | --- |
| 400 | `bad_request`, `invalid_order`, `invalid_amount`, `invalid_interval`, `invalid_username`, `invalid_password` |
| 401 | `unauthorized`, `invalid_credentials` |
| 403 | `forbidden`, `invalid_credentials` (wrong current password on a password change) |
| 404 | `unknown_symbol`, `unknown_account` |
| 409 | `username_taken`, `duplicate_order` |
| 422 | `insufficient_funds`, `insufficient_shares` |
| 429 | `rate_limited` |
| 503 | `market_halted` |
| 500 | `internal_error`. Details are logged, never returned. |

## Running

To deploy, run `./deploy.sh`. It pulls the latest code (`git pull --ff-only`), builds the images, starts the full stack (`t3-postgres`, `t3-server`, `t3-mm-liquidity`, `t3-mm-flow` and `t3-web`), and waits until the server's `/readyz` and the web app report healthy. Use `--skip-pull` to deploy the current checkout. Locally, the web app is at `http://localhost:3000` and the API at `http://localhost:8080`.

Put secrets in a git-ignored `.env` file next to the script:

```sh
T3_ADMIN_PASSWORD=...
T3_MARKET_MAKER_PASSWORD=...
T3_PORT=8080
T3_WEB_PORT=3000
```

Without them, the script warns and falls back to the development passwords in `docker-compose.yml`.

To serve through a reverse proxy or tunnel in another Compose project, set `T3_EDGE_NETWORK` to a shared external Docker network. `deploy.sh` then creates that network if needed, publishes no host ports, and attaches `t3-server` and `t3-web` to it (`docker-compose.edge.yml`). The proxy should give each its own public hostname, for example `t3-api.example.com` → `http://t3-server:8080` and `t3.example.com` → `http://t3-web:8080`. Keep `/metrics` off the public API hostname. The deploy also trusts `CF-Connecting-IP` from that network's subnet, so rate limits apply per real client rather than per proxy. Edge mode needs:

```sh
T3_EDGE_NETWORK=edge
T3_PUBLIC_URL=https://t3.example.com          # the web app
T3_PUBLIC_API_URL=https://t3-api.example.com  # the API, which browsers call directly
T3_ALLOWED_ORIGINS=https://t3.example.com     # plus any other sites that call the API
```

To run the server alone in memory, with no database:

```sh
T3_ADMIN_PASSWORD=change-me go run ./cmd/server
```

| Variable | Default | |
| --- | --- | --- |
| `T3_ADDR` | `:8080` | Listen address |
| `T3_HEARTBEAT` | `10s` | Batch interval |
| `T3_TICKERS_FILE` | — | A JSON listing to use instead of the 10 built-in example tickers (`pkg/listing`) |
| `T3_DATABASE_URL` | — | Postgres URL for every service. If unset (and no per-service URL is set), state is in memory only. |
| `T3_ENGINE_DATABASE_URL`, `T3_LEDGER_DATABASE_URL`, `T3_GATEWAY_DATABASE_URL` | `T3_DATABASE_URL` | Per-service URLs, one role each |
| `T3_MARKET_MAKERS` | `mm-liquidity,mm-flow` | Market maker accounts to bootstrap |
| `T3_MARKET_MAKER_PASSWORD` | — | If unset, no market makers are created |
| `T3_MARKET_MAKER_CASH` | `500000000` | Cents seeded to each market maker; shares come from the listing |
| `T3_STARTING_CASH` | `1000000` | Cents credited to each new trader |
| `T3_ALLOWED_ORIGINS` | — | Comma-separated CORS origins. Compose defaults it to the local web app. |
| `T3_ADMIN_USERNAME` | `admin` | Bootstrap admin username |
| `T3_ADMIN_PASSWORD` | — | If unset, no admin is created |
| `T3_TRUSTED_PROXIES` | — | Comma-separated CIDRs whose connections may name the client in `CF-Connecting-IP`. `deploy.sh` sets it in edge mode. |
| `T3_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `T3_COMPACT_AFTER` | `24h` | How old a day of engine history must be before it is compacted (see [persistence](persistence.md#compaction)). `0` disables compaction. |
| `T3_ARCHIVE_DIR` | — | Where compaction keeps each day's original ticks. Compose sets it to the `t3-archive` volume. If unset, they are discarded. |

## Web app

`web/` is the browser client: a Preact single-page app, built by Vite and served by nginx in the `t3-web` container. Anyone can browse prices, charts and the tape. Signed-in traders also see their account value, positions and fills, and can place orders. The Portfolio page (`#/portfolio`) charts their account value over time, shows their allocation, and lists each holding with its return against its average cost. Anyone can view the Leaderboard page (`#/leaderboard`), which ranks traders by account value. It talks only to the public API above.

The container reads `T3_API_URL` at startup and serves it to the app as `/config.json`, and also allows it in the page's Content Security Policy, so one image works for any deployment. Compose sets it from `T3_PUBLIC_API_URL`.

To work on it with live reload against a running API:

```sh
cd web && npm install && npm run dev   # http://localhost:3000
```

The dev server has no `/config.json`, so it uses `VITE_T3_API_URL`, or `http://localhost:8080` by default. The API must list `http://localhost:3000` in `T3_ALLOWED_ORIGINS`, which is the Compose default.

There is no endpoint to list or cancel open orders yet, so the order ticket defaults to IOC. A GTC limit order keeps its funds held until it fills.

## Market makers

`cmd/marketmaker` runs one market maker per process against this API. Each one is configured with `T3_API_URL`, `T3_MM_USERNAME`, `T3_MM_PASSWORD`, `T3_MM_STRATEGY` (`liquidity` or `flow`), an optional `T3_MM_SEED` and, for `liquidity`, an optional `T3_MM_DEPTH`.

- **`liquidity`** quotes a five-level ladder of IOC bids and asks around every price, from 0.15% to 2.5% away. Each side totals `T3_MM_DEPTH` cents (default $100,000), with about $15,000 within 0.3%, so a new player's whole $10,000 fills near the last price and larger orders fill at a worse price instead of not at all. Its fair value drifts back toward the reference price, and it skews quotes against its inventory, but it stays within 0.5% of the last price.
- **`flow`** is a noise trader. Each tick it crosses the spread on about half the symbols, steered by a slowly wandering sentiment per symbol.

Both send only IOC orders, so nothing they place ever rests in the book.
