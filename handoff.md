# Handoff

State of the work as of 2026-10-23, for picking up in a new conversation.

## Done (uncommitted)

The working tree has two sets of uncommitted changes:

1. **Earlier work, not from this session:** changes in `docs/api.md`, `docs/bots.md`, `future.md`, `pkg/gateway/{gateway,gateway_test,leaderboard}.go`, `pkg/ledger/{ledger,ledger_test}.go`, `web/src/{App.tsx,api.ts}`, `web/src/components/{Leaderboard,Tables,Ticket}.tsx`, and the new `pkg/gateway/orders.go`.
2. **This session: the fixed reference price is removed.** Some of these edits share files with set 1.

### What changed in set 2

- **API:** `GET /v1/market/symbols` returns only `symbol` and `name`. The `reference_price` field is gone, and `TestSymbolsListNoPrice` (`pkg/gateway/gateway_test.go`) checks it stays gone.
- **Listing:** `listing.Ticker.ReferencePrice` is renamed to `OpeningPrice`, with JSON key `opening_price`. It only sets a symbol's price before its first trade, and is used by `cmd/server/main.go`, `pkg/reporting`, `pkg/gateway/admin.go` and the ledger's opening valuations. **Any `T3_TICKERS_FILE` on the server must rename the key, or the server won't start.**
- **Makers:** each now forms its own private reference with a new `Opinion` type in `pkg/marketmaker/marketmaker.go`, embedded in `Liquidity`, `Momentum` and `News`. A reference:
  - starts at the first price the maker sees;
  - follows the price with half-life `Memory` (default 8640 ticks, one day at 10s ticks);
  - wanders by `Drift` each tick (default 0.0003, about 3% a day).
- **What the makers do with it:** `Liquidity` reverts fair value toward its reference, `Momentum` stops chasing 10% from it, and `News` biases stories toward it and caps targets at 20% from it. Each maker's seed differs, so the references disagree. `Flow` is unchanged.
- **Admin settings:** `memory` and `drift` are new tunable settings on those three makers (`pkg/marketmaker/settings.go`), so the admin page shows them automatically.
- **Constructors:** `NewLiquidity`, `NewMomentum` and `NewNews` now take only a seed. `marketmaker.Ticker` is now just `{Symbol}`.
- **Docs:** `docs/api.md` market-maker section updated.
- **Checks:** all Go tests pass (`go test ./pkg/...`) and the web app type-checks.

### Simulated behaviour

These runs were ad hoc with the four makers and no players, and the test file was deleted afterwards.

| Reference memory | Average move per symbol over one day | Range |
|---|---|---|
| 2 hours | 13% | −21% to +27% |
| 1 day (the default) | 7% | −8% to +12% |

That's still more volatile than real stocks, which typically move 1–2% a day. The levers are `memory`, `drift`, flow's `volatility` and news's `rate`. Over the simulated day the news maker's cash fell from about $5M to $3.3M while liquidity's rose, which may be worth watching. That's cash only, not counting inventory value.

## Done (uncommitted): the "secret-link" bot

The link maker is built as `cmd/linkbot` and `pkg/link`, and runs as the Compose service `mm-anchor`. It is documented in `docs/api.md` under "The link maker", and `links.example.json` shows the file format.

- **Sources:** Finnhub for stocks (`T3_FINNHUB_API_KEY`), Coinbase for crypto (no key), Open-Meteo for weather (no key, yesterday's daily value), and FRED for commodities and BLS retail prices (`T3_FRED_API_KEY`).
- **How it moves prices:** an anchor closes on each day's target over `half_life_days`. Heavy IOC walls sit `band` (±3%) either side of the anchor and push the price along.
- **Cash:** the `mm-anchor` account is seeded with $25M through the new `name=cents` syntax in `T3_MARKET_MAKERS`. Its shares are the normal listing seed, which limits how hard it can push a price down.
- **State:** saved in the `t3-link` volume.

## Also open

- Earlier the user asked for a document describing the desired behaviour of each market maker. The current behaviour is summarised in `docs/api.md`. A dedicated `docs/makers.md` was suggested but not written.
- News articles still publish `target`, shown as a percentage move in `web/src/components/News.tsx`. That's fine now, since it's only the news maker's own opinion, but it should be reconsidered if a link drives news.
