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
| `GET /v1/account/portfolio` | trader | Holdings valued at last prices. Each position's `cost_basis` is what its shares cost at their average purchase price, or `null` if some predate the retained trade history (such as deposited shares). `pool_value` is the account's stake in the lending pool and `owed` what it owes the pool; `total_value` is cash plus holdings plus `pool_value`, less `owed`. `status` is `active`, `liquidating` or `bankrupt`. |
| `GET /v1/account/trades?limit=` | trader | Your own fills, newest first. |
| `GET /v1/account/history?interval=&limit=` | trader | Your account's total value (cash plus holdings at last prices) as `open`/`high`/`low`/`close` candles, oldest first, with the same `interval` options as market candles. See below. |
| `GET /v1/orders` | trader | Your open orders, oldest first, as `{orders}`. See [Open orders](#open-orders). |
| `POST /v1/orders` | trader | Places an order, returning `202` with the accepted order. See below. |
| `DELETE /v1/orders/{id}` | trader | Cancels one of your open orders at the next auction, returning `202` with the order, now `cancelling`. See [Open orders](#open-orders). |
| `GET /v1/leaderboard?limit=` | — | Traders ranked by account value, highest first, as `{tick, starting_cash, traders, standings, you}`. Each standing has `rank`, `username`, `total_value`, `gain` (value less starting cash) and `pool_value` (what the trader's stake in the lending pool is worth, which `total_value` includes), and equal values share a rank. `you` is the caller's own standing when a trader token is sent, even outside the limit, and `null` otherwise. Admins and market makers are not ranked. Standings are computed once per tick, so a new trader appears after the next auction. |
| `GET /v1/leaderboard/bankruptcies?limit=` | — | Traders closed by liquidation, as `{bankruptcies, total}`, most unpaid debt first. Each has `username`, `tick`, `time` and `written_off`, the debt the pool's lenders lost. Bankrupt traders are left out of `GET /v1/leaderboard`. |
| `GET /v1/pool` | — | The lending pool as `{pool, you}`. See [Leverage](#leverage). `you` is the caller's `{status, cash_available, pool_shares, pool_value, owed}` when a token is sent, and `null` otherwise. |
| `POST /v1/pool/lend` | trader | `{amount}`: moves cash into the pool for pool shares. Returns what `GET /v1/pool` does. An account that owes the pool can't lend (`422 leverage_not_allowed`). |
| `POST /v1/pool/withdraw` | trader | `{amount}` or `{all: true}`: pays cash out of your stake. Only cash the pool hasn't lent out can be withdrawn (`422 insufficient_pool`). |
| `POST /v1/pool/repay` | trader | `{amount}`: repays up to that much of what you owe from cash not held for orders. |
| `GET /v1/news?symbol=&limit=` | — | Market news, newest first, as `{articles}`. Each article has `id`, `tick`, `time`, `symbol`, `headline`, an optional `body`, the symbol's `price` when the story broke and the `target` price it moves towards, or `0` for admin news. `symbol` filters to one symbol. |
| `POST /v1/news` | market maker | `{symbol, headline, body?, price, target}`: publishes an article, returning `201` with it. The headline is 1–200 characters and the body at most 4,000. The `news` market maker calls this for each story it starts. Admins may also publish. |
| `POST /v1/news/alerts/claim` | market maker | Takes the queued admin news as `{alerts}`, each `{symbol, quantity}`. The `news` market maker claims these every tick and places a market buy for each. The queue is in memory, so unclaimed alerts are lost on restart. |
| `POST /v1/maker/sync` | market maker | `{strategy, settings}`: reports the maker's strategy and its tunable settings, each `{key, label, unit, default, min, max, integer, help?}`, and returns what admins have set as `{paused, overrides}`. Every market maker calls this once a tick, before trading. See [Market makers](#market-makers). |
| `GET /v1/market/symbols` | — | The tradable symbols, each with `name`. |
| `GET /v1/market/prices` | — | Every symbol's current price, as one pre-built snapshot that changes once per tick. It supports `ETag`/`If-None-Match`, which returns `304` while unchanged. |
| `GET /v1/market/{symbol}/quote` | — | Last clearing price. Returns `404` if the symbol hasn't traded yet. |
| `GET /v1/market/{symbol}/trades?limit=` | — | The public tape, newest first, with no account or order IDs. |
| `GET /v1/market/{symbol}/candles?interval=&limit=` | — | OHLCV candles, oldest first. `interval` is one of `1m`, `5m`, `15m`, `1h` or `1d`. |
| `GET /v1/admin/users` | admin | Every trader as `{users}`, each with `id`, `username`, `status`, `blocked`, `cash`, `cash_held` and `total_value`. |
| `POST /v1/admin/accounts/{id}/cash` | admin | `{amount}`: credits cash, or debits it when negative. A debit can't exceed the cash not held for open orders (`422 insufficient_funds`). Returns the account. |
| `POST /v1/admin/news` | admin | `{symbol, quantity, headline, body?}`: publishes an article and queues an alert for the `news` market maker to market buy `quantity` shares (1–1,000,000) in the next auction, returning `201` with the article, whose `target` is `0`. The web app's `#/admin` page uses this. |
| `POST /v1/admin/accounts/{id}/shares` | admin | `{symbol, quantity}`: credits shares. |
| `POST /v1/admin/accounts/{id}/block` | admin | `{blocked}`: blocks a trader from placing orders and lending (`403 account_blocked`), cancelling their open orders in the next auction, or lifts the block. Returns the account. |
| `POST /v1/admin/accounts/{id}/liquidate` | admin | Liquidates a trader as a margin call would: their open orders are cancelled and all their shares market sold, with proceeds repaying any loan. They can't trade until it finishes, then return to `active` (or `bankrupt` if they still owe the pool). Returns the account. |
| `DELETE /v1/admin/users/{id}` | admin | Deletes a trader: their sessions and API token stop working, they leave the leaderboard, and their ledger account is blocked, which cancels their open orders. The ledger account and its history are kept. `204`. |
| `GET /v1/admin/makers` | admin | Every market maker as `{makers}`, each with `id`, `username`, `strategy`, `online` (synced in the last minute), `seen_at`, `paused`, `cash`, `cash_held`, `total_value`, `holdings` and `settings`. Each setting is as the maker reported it, plus the `value` it trades with and whether that is `overridden`. `strategy` is empty and `settings` empty until the maker first syncs after a server start. Cash and shares are adjusted with the `/v1/admin/accounts/{id}` endpoints above, which work on any account. |
| `POST /v1/admin/makers/{id}/control` | admin | `{paused?, overrides?}`: pauses or resumes a market maker, or replaces its setting overrides (`{}` clears them). Omitted fields are unchanged. Each override must name a setting the maker reported and be within its range (`400`); overrides can't be set before the maker has synced (`409 maker_offline`). Controls are stored in the gateway's database and the maker applies them from its next tick. Returns the maker. |
| `POST /v1/admin/makers/{id}/orders` | admin | Places an order for a market maker, in the shape of `POST /v1/orders` without `leverage`, returning `202`. It defaults to `IOC`. A `MARKET` `BUY` without `max_cost` may spend up to twice its quantity at the last price, capped by the maker's available cash. |
| `GET /healthz` | — | Liveness check. Not rate limited. |
| `GET /readyz` | — | Returns `503 market_halted` while the ledger is too far behind to settle trades. |
| `GET /metrics` | — | Prometheus counters. Keep this on an internal network. |

## Placing an order

```json
{"symbol": "ACME", "direction": "BUY", "type": "LIMIT", "quantity": 10, "limit_price": 1010}
```

`max_cost` is required on market buys. `time_in_force` is `GTC` (the default) or `IOC`; any unfilled part of an `IOC` order expires after one auction. A buy may set `leverage` to borrow from the lending pool; see [Leverage](#leverage). A client can't set `id`, `account_id` or `sequence`; the gateway assigns the ID, the account comes from the token, and the engine assigns the sequence.

The gateway reserves the order's funding with the ledger before the engine sees the order. If the engine rejects it, the hold is released. The order fills at the next heartbeat.

Order entry returns `503 market_halted` if the ledger falls more than 3 ticks behind the engine, because fills could no longer be settled. Market data stays available.

## Open orders

An order is open from when it is accepted until it fills or expires. Each open order has the fields it was placed with (`id`, `symbol`, `direction`, `type`, `quantity`, `limit_price`, `max_cost`, `leverage`, and `time_in_force` on limit orders), plus:

| Field | |
| --- | --- |
| `remaining` | Shares still unfilled. |
| `cash_held`, `shares_held` | What the order still sets aside, which cancelling it frees. A leveraged buy's `cash_held` is only your own share. |
| `created_at` | When it was placed. |
| `status` | `open`, or `cancelling` once you have asked to cancel it. |

`DELETE /v1/orders/{id}` asks the engine to remove the order before the next auction, and its funds are released when that auction settles. If the request arrives while an auction is clearing, any part that fills in that auction still settles, and the rest is removed in the one after. Cancelling is idempotent. An ID that isn't one of your open orders returns `404 unknown_order`, as do other traders' orders, and an account being liquidated can't cancel (`409 account_liquidating`), since liquidation cancels its orders itself. Cancelling still works while order entry is halted.

A cancellation the server restarts before applying is lost, and the order shows as `open` again: cancel it again.

## Leverage

Traders lend cash to a shared pool, and other traders borrow from it to buy with leverage. `GET /v1/pool` returns:

| Field | |
| --- | --- |
| `value` | Everything the pool has: `cash` plus `lent` |
| `cash`, `available` | Cash not lent out, and the part of it not set aside for open orders, which can be borrowed or withdrawn |
| `lent` | What borrowers owe, interest included |
| `utilization_bps` | `lent` as a share of `value`, in basis points |
| `lender_rate_bps` | What lenders earn now, in basis points per day: the interest borrowers pay, spread over the whole pool |
| `base_rate_bps`, `rates` | The borrowing rate, and each allowed `{ratio, rate_bps}`, in basis points per day |
| `maintenance_margin_bps` | The equity, as a share of holdings, below which a borrower is liquidated |
| `max_trade_loan`, `max_trade_loan_bps` | The most one order may borrow now, and that limit as a share of `value` |
| `max_trader_loan`, `max_trader_loan_bps` | The most one trader may owe now, and that limit as a share of `value` |
| `lenders`, `borrowers` | How many accounts lend and borrow |

**Lending.** A deposit buys pool shares at the current share price, the pool's value divided by its shares. Interest raises the pool's value, so every lender earns in proportion to their stake. Losses from bankruptcies lower it in the same way: lenders carry the risk, and nothing protects them.

**Borrowing.** A buy with `"leverage": 2`, `3` or `5` funds `1/leverage` of its cost from the account's cash and borrows the rest as it fills. The pool must have that much `available` (`422 insufficient_pool`). One order may borrow at most 5% of the pool's value, and one trader may owe at most 50% of it, counting loans set aside for their open orders (`422 loan_limit`; set by `T3_MAX_TRADE_LOAN_BPS` and `T3_MAX_TRADER_LOAN_BPS`). What the account would owe can't exceed `leverage - 1` times its equity, and an account with a stake in the pool can't borrow (both `422 leverage_not_allowed`). Debt belongs to the account, not the purchase: every sale repays it first, most expensive ratio first, and `POST /v1/pool/repay` repays from cash.

**Interest** accrues every tick. The base rate is 5% a day (`T3_LENDING_RATE_BPS`), plus 1% at 3x and 2% at 5x. Once more than 80% of the pool is lent, the base rate climbs, to three times its normal level when the pool is fully lent.

**Liquidation.** After every tick, an account whose equity (cash, plus holdings at last prices, less what it owes) falls below 15% of its holdings is liquidated. Its `status` becomes `liquidating`, it can no longer trade (`409 account_liquidating`), its open orders are cancelled and its shares are sold with market orders, auction after auction, until none are left. Its cash then repays what it can. If that covers the debt, the account becomes `active` again. Otherwise the rest is written off, which every lender shares, and the account is closed for good as `bankrupt` (`403 account_closed`).

## Account history

History is rebuilt by rewinding the account's current balances through its fills, so it starts at the oldest retained fill and ends with the period of the latest tick. Every period in between appears, flat if nothing changed. Values are sampled once a minute at each symbol's closing price, so `high` and `low` are accurate to that resolution. Deposits are not fills, so they appear to have been there from the start, as do the account's current pool stake and debt.

## Error codes

| Status | Codes |
| --- | --- |
| 400 | `bad_request`, `invalid_order`, `invalid_amount`, `invalid_interval`, `invalid_username`, `invalid_password` |
| 401 | `unauthorized`, `invalid_credentials` |
| 403 | `forbidden`, `invalid_credentials` (wrong current password on a password change), `account_closed` |
| 404 | `unknown_symbol`, `unknown_account`, `unknown_order` |
| 409 | `username_taken`, `duplicate_order`, `account_liquidating` |
| 422 | `insufficient_funds`, `insufficient_shares`, `insufficient_pool`, `loan_limit`, `leverage_not_allowed` |
| 429 | `rate_limited` |
| 503 | `market_halted` |
| 500 | `internal_error`. Details are logged, never returned. |

## Running

To deploy, run `./deploy.sh`. It pulls the latest code (`git pull --ff-only`), builds the images, starts the full stack (`t3-postgres`, `t3-server`, `t3-mm-liquidity`, `t3-mm-flow`, `t3-mm-momentum`, `t3-mm-news`, `t3-mm-anchor` and `t3-web`), and waits until the server's `/readyz` and the web app report healthy. Use `--skip-pull` to deploy the current checkout. Locally, the web app is at `http://localhost:3000` and the API at `http://localhost:8080`.

Put settings in a git-ignored `.env` file next to the script:

```sh
T3_ADMIN_PASSWORD=...
T3_MARKET_MAKER_PASSWORD=...
T3_PORT=8080
T3_WEB_PORT=3000
T3_STARTING_CASH=1000000
T3_LENDING_RATE_BPS=500
```

On a first deploy, before the `t3-pgdata` volume exists, the script generates any missing password and saves it to `.env` (mode `600`). It can't do that later, because the admin and market makers keep the password they were first created with. On an existing install, a missing password makes the script warn and fall back to the development password in `docker-compose.yml`. Changing one later means changing the stored password too.

The server also receives `T3_ADMIN_USERNAME`, `T3_STARTING_CASH`, `T3_LENDING_RATE_BPS`, `T3_MAX_TRADE_LOAN_BPS`, `T3_MAX_TRADER_LOAN_BPS`, `T3_MARKET_MAKER_CASH`, `T3_COMPACT_AFTER` and `T3_LOG_LEVEL` from `.env`; unset ones use the defaults in the table below. The script rejects malformed numbers and checks the Compose configuration before building anything.

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
| `T3_TICKERS_FILE` | — | A JSON listing to use instead of the 10 built-in example tickers (`pkg/listing`): an array of `symbol`, `name`, `opening_price` (cents, before the first trade) and `maker_shares` |
| `T3_DATABASE_URL` | — | Postgres URL for every service. If unset (and no per-service URL is set), state is in memory only. |
| `T3_ENGINE_DATABASE_URL`, `T3_LEDGER_DATABASE_URL`, `T3_GATEWAY_DATABASE_URL` | `T3_DATABASE_URL` | Per-service URLs, one role each |
| `T3_MARKET_MAKERS` | `mm-liquidity,mm-flow,mm-momentum,mm-news,mm-anchor=2500000000` | Market maker accounts to bootstrap. `name=cents` seeds that maker with its own cash instead of `T3_MARKET_MAKER_CASH`; seeding happens once, so changing it later has no effect. |
| `T3_MARKET_MAKER_PASSWORD` | — | If unset, no market makers are created |
| `T3_MARKET_MAKER_CASH` | `500000000` | Cents seeded to each market maker; shares come from the listing |
| `T3_STARTING_CASH` | `1000000` | Cents credited to each new trader |
| `T3_LENDING_RATE_BPS` | `500` | The lending pool's base borrowing rate, in basis points per day |
| `T3_MAX_TRADE_LOAN_BPS` | `500` | The most one order may borrow, in basis points of the lending pool's value |
| `T3_MAX_TRADER_LOAN_BPS` | `5000` | The most one trader may owe the lending pool, in basis points of its value |
| `T3_ALLOWED_ORIGINS` | — | Comma-separated CORS origins. Compose defaults it to the local web app. |
| `T3_ADMIN_USERNAME` | `admin` | Bootstrap admin username |
| `T3_ADMIN_PASSWORD` | — | If unset, no admin is created |
| `T3_TRUSTED_PROXIES` | — | Comma-separated CIDRs whose connections may name the client in `CF-Connecting-IP`. `deploy.sh` sets it in edge mode. |
| `T3_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `T3_COMPACT_AFTER` | `24h` | How old a day of engine history must be before it is compacted (see [persistence](persistence.md#compaction)). `0` disables compaction. |
| `T3_ARCHIVE_DIR` | — | Where compaction keeps each day's original ticks. Compose sets it to the `t3-archive` volume. If unset, they are discarded. |

## Web app

`web/` is the browser client: a Preact single-page app, built by Vite and served by nginx in the `t3-web` container. Anyone can browse prices, charts and the tape. Signed-in traders also see their account value, positions and fills, and can place orders. The Portfolio page (`#/portfolio`) charts their account value over time, shows their allocation, and lists each holding with its return against its average cost. Anyone can view the Leaderboard page (`#/leaderboard`), which ranks traders by account value, the News page (`#/news`), which lists market news and can filter it by symbol, and the Pool page (`#/pool`), which shows the lending pool's size and rates and lets signed-in traders lend, withdraw and repay. It talks only to the public API above.

The container reads `T3_API_URL` at startup and serves it to the app as `/config.json`, and also allows it in the page's Content Security Policy, so one image works for any deployment. Compose sets it from `T3_PUBLIC_API_URL`.

To work on it with live reload against a running API:

```sh
cd web && npm install && npm run dev   # http://localhost:3000
```

The dev server has no `/config.json`, so it uses `VITE_T3_API_URL`, or `http://localhost:8080` by default. The API must list `http://localhost:3000` in `T3_ALLOWED_ORIGINS`, which is the Compose default.

The order ticket defaults to IOC. A GTC limit order keeps its funds held until it fills or the trader cancels it from the Open orders tab under the chart.

## Market makers

`cmd/marketmaker` runs one market maker per process against this API. Each one is configured with `T3_API_URL`, `T3_MM_USERNAME`, `T3_MM_PASSWORD`, `T3_MM_STRATEGY` (`liquidity`, `flow`, `momentum` or `news`), an optional `T3_MM_SEED` and, for `liquidity`, an optional `T3_MM_DEPTH`.

- **`liquidity`** quotes a five-level ladder of IOC bids and asks around every price, from 0.15% to 2.5% away. Each side totals `T3_MM_DEPTH` cents (default $100,000), with about $15,000 within 0.3%, so a new player's whole $10,000 fills near the last price and larger orders fill at a worse price instead of not at all. Its fair value drifts back toward its own reference price, and it skews quotes against its inventory, but it stays within 0.5% of the last price.
- **`flow`** is a noise trader. Each tick it crosses the spread on about half the symbols, steered by a slowly wandering sentiment per symbol.
- **`momentum`** follows trends. It compares a fast and a slow moving average of each price and trades up to $5,000 in the direction of the gap, so moves that start tend to run for a while. It stops chasing once a price is 10% from its own reference price.
- **`news`** trades on imaginary headlines. About once every 100 ticks per symbol, a story sets a target 2–8% away, and it trades about $20,000 a tick towards it for 3–10 ticks. Stories are likelier to push a price back towards the maker's own reference price, and never target more than 20% from it. It publishes each story to `POST /v1/news` before trading on it. For now the headline only states the move, such as "ACME soars 5.2%"; `News.Write` is the hook for writing richer articles.

Nothing anchors prices. Makers see only what players see, and `liquidity`, `momentum` and `news` each form their own private reference price per symbol from it. A reference starts at the first price the maker sees, follows the price with a half-life of a day (8,640 ticks, the `memory` setting), and wanders by about 3% a day on its own (the `drift` setting). Each maker's reference differs from the others' and keeps moving, so prices trend and drift over days instead of returning to a fixed value. References live only in the maker's memory, so a restarted maker starts again from the current prices.

`momentum` and `news` cross the spread by at most 0.4% and 0.6%, inside the 1% a player's order needs to fill in full, so a player's order at that price has priority over theirs in the auction.

All of them send only IOC orders, so nothing they place ever rests in the book.

### The link maker

`cmd/linkbot` is a separate maker, `t3-mm-anchor`, that ties chosen symbols to outside values. It shares nothing with the other makers. Each link maps a symbol to one of four sources, all free:

| Source | API | Key |
| --- | --- | --- |
| `stock` | [Finnhub](https://finnhub.io) quote for `ticker` | `T3_FINNHUB_API_KEY` |
| `crypto` | [Coinbase](https://docs.cdp.coinbase.com/coinbase-app/docs/api-prices) spot price for `pair`, e.g. `BTC-USD` | none |
| `weather` | [Open-Meteo](https://open-meteo.com) daily `variable` (default `temperature_2m_mean`) at `latitude`, `longitude`, for yesterday | none |
| `commodity` | [FRED](https://fred.stlouisfed.org) series' latest value, e.g. `DCOILWTICO` (oil, daily), `PCOPPUSDM` (copper, monthly), or a BLS average retail price | `T3_FRED_API_KEY` |

Once a day at `fetch_at` (UTC), it fetches each value. The first value maps to the symbol's price at that moment, or to `base_price`. Each later value sets a target price:

- in `ratio` mode (the default), base price × (value ÷ first value) ^ `sensitivity`;
- in `linear` mode (the default for weather), base price × (1 + `sensitivity` × (value − first value)). For example, a sensitivity of `0.01` means 1% per degree.

A day's target moves at most `max_daily_move` (15%) from the last one. The maker keeps an anchor that closes half its gap to the target every `half_life_days`. Every tick, it trades up to `pull` cents (default $1,000) towards the anchor, in proportion to how far the price is from it as a fraction of `band`. That is too small to beat the other makers' noise from tick to tick, but it sets the long-term trend. It also quotes walls of `depth` cents (default $300,000) a side: bids `band` (25%) below the anchor and asks 25% above, a backstop the price rarely reaches. If the price is outside the corridor, the walls start at most `max_lean` (0.5%) past it, so the price is pushed back over several ticks rather than all at once. The maker buys low and sells high at its walls, so the corridor roughly pays for itself. To push a price down, though, it needs shares to sell, and it has only its listing seed.

The mapping file is secret. Copy it by hand to `links/links.json` next to `docker-compose.yml`, readable by uid 65532 (`chmod 644`). `links/` is ignored by git and Docker builds. See `links.example.json` for the format. The maker re-reads the file every 15 minutes and fetches new links at once. If the file or one of its entries is malformed, it keeps the last good version and logs a warning. Bases, targets and anchors live in the `t3-link` volume, so a new file only re-bases a symbol whose source changed. Targets appear only in this container's logs. Admins see the maker as `link` with no settings, and can pause it.

Admins control them from the `#/admin` page. Each tick, before trading, a maker calls `POST /v1/maker/sync` with its strategy and tunable settings, whose defaults are the values above (and `T3_MM_DEPTH`). The response says whether it is paused and which settings admins have overridden. A paused maker places no orders and claims no news alerts, so admin news waits until it resumes. Overrides apply from that tick; clearing one returns the setting to the maker's own default. If a sync fails, the maker keeps the settings it had. From the same page admins can add or remove a maker's cash and force it to buy or sell.
