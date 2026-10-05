# Future Features

Features we want to add eventually. Nothing here is scheduled yet.

## Leverage trading

Let traders borrow to take positions larger than their cash, so gains and losses are magnified.

Open questions:

- **Ledger:** `Reserve` currently refuses any order the account can't fully fund. Margin needs a borrowed balance per account and a maximum leverage ratio.
- **Margin calls and liquidation:** decide what happens when an account's equity falls below its maintenance margin, for example a forced sell in the next auction.
- **Interest:** decide whether borrowed money accrues interest each tick or each day.
- **Short selling:** decide whether leverage also covers selling shares the account doesn't hold.
- **Portfolio and leaderboard:** account value must subtract what is owed, so leverage can't inflate a ranking.

## Leaderboard: Biggest Winners and Biggest Losers

The leaderboard ranks traders by total account value, which works because everyone starts with the same cash. We also want to rank by **recent gain**, with two tabs:

- **Biggest Winners:** the largest gains over a recent window.
- **Biggest Losers:** the largest losses over the same window.

Open questions:

- **Window:** pick the period, such as the last 24 hours, the current UTC day or the last 7 days, or offer a selector.
- **Absolute or relative gain:** dollar gains favour large accounts, so percentage gain is probably fairer.
- **Data:** the gateway computes standings from current values only. Recent gain needs each trader's value at the start of the window. That could come from `AccountHistory` in the reporting service, or from a value snapshot stored at each window boundary, which would be cheaper for many traders.
- **Deposits:** admin credits are not trading gains and should be excluded.
