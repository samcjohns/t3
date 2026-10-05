#!/usr/bin/env python3
"""A minimal t3 trading bot, using only the Python standard library.

It trades a simple mean-reversion rule: buy a symbol when its price drops a
little below its recent average, and sell when it rises a little above it.
It's an example of using the API, not a good strategy.

    T3_API_URL=https://t3-api.example.com T3_API_TOKEN=t3_... python3 simple_bot.py

See docs/bots.md for how the API works.
"""

import json
import os
import re
import time
import urllib.error
import urllib.request
from collections import defaultdict, deque
from datetime import datetime, timezone

API = os.environ.get("T3_API_URL", "http://localhost:8080").rstrip("/")
TOKEN = os.environ["T3_API_TOKEN"]

WINDOW = 12  # auctions in the moving average
BAND = 0.005  # trade when 0.5% away from it
SPEND = 50_000  # cents to spend per buy ($500)


class ApiError(Exception):
    def __init__(self, status, code, message):
        super().__init__(f"{status} {code}: {message}")
        self.status, self.code = status, code


def call(method, path, body=None):
    """Sends one API request, waiting out rate limits. Money is in cents."""
    data = None if body is None else json.dumps(body).encode()
    headers = {"Authorization": f"Bearer {TOKEN}", "Content-Type": "application/json"}
    while True:
        req = urllib.request.Request(API + path, data=data, method=method, headers=headers)
        try:
            with urllib.request.urlopen(req, timeout=10) as res:
                return None if res.status == 204 else json.load(res)
        except urllib.error.HTTPError as e:
            if e.code == 429:
                time.sleep(int(e.headers.get("Retry-After", "1")))
                continue
            try:
                err = json.loads(e.read()).get("error", {})
            except ValueError:  # not from the API, e.g. a proxy error page
                err = {}
            raise ApiError(e.code, err.get("code"), err.get("message") or e.reason) from None


def parse_time(s):
    # The API sends RFC 3339 times with up to nanosecond precision; Python
    # parses at most microseconds.
    s = re.sub(r"(\.\d{6})\d+", r"\1", s).replace("Z", "+00:00")
    return datetime.fromisoformat(s)


def wait_for_next_auction(snapshot):
    """Sleeps until just after the next auction clears."""
    next_at = parse_time(snapshot["next_tick_at"])
    delay = (next_at - datetime.now(timezone.utc)).total_seconds()
    time.sleep(max(delay, 0) + 0.5)


def main():
    history = defaultdict(lambda: deque(maxlen=WINDOW))
    last_tick = -1
    print(f"Trading on {API}")
    while True:
        snap = call("GET", "/v1/market/prices")
        if snap["tick"] == last_tick:
            time.sleep(1)  # the auction is running late
            continue
        last_tick = snap["tick"]

        portfolio = call("GET", "/v1/account/portfolio")
        cash = portfolio["cash"] - portfolio["cash_held"]
        # Shares already reserved by open orders can't be sold again.
        owned = {p["symbol"]: p["quantity"] - p["held"] for p in portfolio["positions"]}

        for p in snap["prices"]:
            symbol, price = p["symbol"], p["price"]
            prices = history[symbol]
            prices.append(price)
            if len(prices) < WINDOW:
                continue
            average = sum(prices) / len(prices)

            order = None
            if price < average * (1 - BAND) and cash >= SPEND:
                quantity = SPEND // price
                # IOC: anything not filled at this auction expires, so no
                # order is left holding cash. The limit caps what we pay.
                order = {"direction": "BUY", "quantity": quantity, "limit_price": int(price * 1.01)}
            elif price > average * (1 + BAND) and owned.get(symbol, 0) > 0:
                order = {"direction": "SELL", "quantity": owned[symbol], "limit_price": int(price * 0.99)}
            if order is None:
                continue

            order.update(symbol=symbol, type="LIMIT", time_in_force="IOC")
            try:
                placed = call("POST", "/v1/orders", order)
                print(f"tick {last_tick}: {order['direction']} {order['quantity']} {symbol} "
                      f"up to ${order['limit_price'] / 100:.2f} ({placed['id']})")
                if order["direction"] == "BUY":
                    cash -= order["quantity"] * order["limit_price"]
            except ApiError as e:
                print(f"tick {last_tick}: {symbol} order rejected: {e}")

        print(f"tick {last_tick}: account value ${portfolio['total_value'] / 100:,.2f}")
        wait_for_next_auction(snap)


if __name__ == "__main__":
    main()
