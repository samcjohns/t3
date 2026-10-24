# Future Features

Features we want to add eventually. Nothing here is scheduled yet.

## Leverage trading: follow-ups

Leverage and the lending pool are built (see [docs/api.md](docs/api.md#leverage)). Still open:

- **New account after bankruptcy:** decide whether the player can start again with a fresh account, and whether that account is marked as a restart.
- **Empty pool:** nobody can borrow until players lend, so leverage is unavailable at launch. Decide whether that is acceptable or whether early lenders get a bonus rate.
- **Per-symbol leverage limits:** thinly traded symbols should get a lower maximum ratio, so one liquidation can't crash the price and set off others.
- **Short selling:** the pool lends cash only. Shorting needs a separate pool that lends shares.

## Leaderboard: Biggest Winners and Biggest Losers

The leaderboard ranks traders by total account value, which works because everyone starts with the same cash. We also want to rank by **recent gain**, with two tabs:

- **Biggest Winners:** the largest gains over a recent window.
- **Biggest Losers:** the largest losses over the same window.

Open questions:

- **Window:** pick the period, such as the last 24 hours, the current UTC day or the last 7 days, or offer a selector.
- **Absolute or relative gain:** dollar gains favour large accounts, so percentage gain is probably fairer.
- **Data:** the gateway computes standings from current values only. Recent gain needs each trader's value at the start of the window. That could come from `AccountHistory` in the reporting service, or from a value snapshot stored at each window boundary, which would be cheaper for many traders.
- **Deposits:** admin credits are not trading gains and should be excluded.
