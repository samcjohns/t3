# Building a trading bot

Anyone with a t3 account can trade from their own code: a script, a bot, or an agent. This guide covers everything a bot needs. The full endpoint reference is in [api.md](api.md).

If you're building with an AI assistant such as Claude, give it this file and `api.md`; between them they describe the whole API.

## How t3 trades

- **Batch auctions:** t3 doesn't match orders as they arrive. Every few seconds (one *tick*, 10 seconds by default), it collects all orders for each symbol and clears them together in a single auction, at a single price. `next_tick_at` in `/v1/market/prices` says when the next one clears. If more shares are wanted than offered, orders fill by best price first, and arrival order only breaks ties between equal prices.
- **Clearing price:** each symbol's price is chosen from the limit prices in its batch, to trade as many shares as possible. Market orders always take part but never set the price, so a batch needs at least one limit order to trade. Market makers quote several levels on both sides of every symbol each tick, about $100,000 per side, so an order priced within 1% of the last price normally fills in full.
- **Money:** amounts are virtual US dollars, always in **integer cents**. `12345` means $123.45.
- **No shorting or leverage:** you can only buy with cash you have and sell shares you own.

## 1. Get an API token

Sign in to the web app, click your username at the top right, and choose **Create API token**. Copy it right away, because it's shown only once. The same popup shows the API's base URL.

An API token starts with `t3_` and works until you regenerate or revoke it. It can read your account and trade, but it can't change your password or manage tokens, so a leaked token can't lock you out. If one leaks, regenerate it from the popup and the old one stops working at once.

Keep it out of your code. Pass it in an environment variable instead:

```sh
export T3_API_URL=https://t3-api.example.com   # the base URL shown in the popup
export T3_API_TOKEN=t3_...
```

You can also create a token with the API, using a session from logging in:

```sh
SESSION=$(curl -s -X POST "$T3_API_URL/v1/auth/login" -d '{"username":"you","password":"..."}' | jq -r .token)
curl -s -X POST "$T3_API_URL/v1/account/api-token" -H "Authorization: Bearer $SESSION"
```

## 2. Make requests

Send the token on every request as a bearer token. Bodies are JSON.

```sh
curl -s "$T3_API_URL/v1/account/portfolio" -H "Authorization: Bearer $T3_API_TOKEN"

curl -s -X POST "$T3_API_URL/v1/orders" -H "Authorization: Bearer $T3_API_TOKEN" \
  -d '{"symbol":"ACME","direction":"BUY","type":"LIMIT","quantity":10,"limit_price":4250,"time_in_force":"IOC"}'
```

The endpoints a bot uses:

| Endpoint | What for |
| --- | --- |
| `GET /v1/market/prices` | Every symbol's latest price, plus `tick` and `next_tick_at`, when the next auction clears. No token needed. |
| `GET /v1/market/{symbol}/candles?interval=1m&limit=100` | Price history. `interval` is `1m`, `5m`, `15m`, `1h` or `1d`. |
| `GET /v1/market/{symbol}/trades` | The recent public trades for a symbol. |
| `GET /v1/account/portfolio` | Your cash, holdings and their value. |
| `GET /v1/account/trades` | Your fills, newest first, each with the `order_id` it filled. |
| `GET /v1/account/history?interval=1m` | Your account value over time. |
| `POST /v1/orders` | Place an order. |
| `GET /v1/leaderboard` | Where you rank against everyone else. |

## 3. Place orders

```json
{"symbol": "ACME", "direction": "BUY", "type": "LIMIT", "quantity": 10, "limit_price": 4250, "time_in_force": "IOC"}
```

| Field | |
| --- | --- |
| `symbol` | One of the symbols in `/v1/market/prices`. |
| `direction` | `BUY` or `SELL`. |
| `type` | `LIMIT` (needs `limit_price`, the worst price you accept) or `MARKET` (any price). |
| `quantity` | Whole shares, more than 0. |
| `limit_price` | Cents. Only for `LIMIT` orders. |
| `max_cost` | Cents. Required on `MARKET` `BUY` orders, and not allowed otherwise. It caps the total spend, and the order fills only as many shares as it covers. |
| `time_in_force` | `IOC` or `GTC` (the default). `IOC` means anything unfilled after the next auction is cancelled. A `GTC` order rests until it fills. |

An accepted order returns `202` with its `id`. It hasn't traded yet: it trades at the next auction. To see what filled, read `GET /v1/account/trades` after that auction and match fills by `order_id`. An order may fill in part.

When you place an order, t3 sets aside what it could need: cash for a buy (`quantity × limit_price`, or `max_cost`) or shares for a sell. Those show as `cash_held` and `held` in the portfolio, and you can't spend them twice. Whatever isn't used is released once the order is done.

**Use `IOC`.** There's no endpoint to list or cancel open orders yet, so a `GTC` order that doesn't fill keeps its cash or shares locked until it does.

## 4. Time your loop to the auctions

Prices change only when an auction clears, so polling faster than once a tick wastes requests. A typical loop:

1. `GET /v1/market/prices`. If `tick` hasn't changed since last time, the auction is running late: wait a second and retry.
2. Read your portfolio and decide what to trade.
3. Place orders. They join the next auction.
4. Sleep until just after `next_tick_at`, then repeat.

`/v1/market/prices` supports `ETag`: send the last `ETag` back as `If-None-Match` to get a cheap `304` while nothing has changed.

## 5. Handle limits and errors

Errors return a status and `{"error": {"code": "...", "message": "..."}}`. The ones a bot should expect:

| Status | Code | What to do |
| --- | --- | --- |
| `429` | `rate_limited` | Wait for the `Retry-After` header's seconds, then retry. Each account gets 10 requests a second, with bursts of 20. |
| `422` | `insufficient_funds`, `insufficient_shares` | The order needs more cash or shares than you have free. Remember that open orders hold some of both. |
| `400` | `invalid_order`, `bad_request` | Fix the order. The message says what's wrong. Unknown JSON fields are rejected. |
| `401` | `unauthorized` | The token is wrong, regenerated or revoked. |
| `403` | `forbidden` | That endpoint needs a signed-in session, not an API token. |
| `503` | `market_halted` | Settlement is behind, so new orders are refused for now. Retry after the next auction. Market data still works. |

## Example bot

[examples/simple_bot.py](examples/simple_bot.py) is a complete bot in about 120 lines of standard-library Python. It waits for each auction, keeps a moving average per symbol, and places `IOC` limit orders when a price moves away from it. It handles rate limits and errors, and is a starting point to replace with your own strategy.

```sh
T3_API_URL=... T3_API_TOKEN=t3_... python3 docs/examples/simple_bot.py
```
