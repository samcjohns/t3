// Package marketmaker implements internal market makers: clients of the
// public API that keep every symbol liquid and its price moving without any
// player activity.
//
// Each maker waits for a new tick, then places immediate-or-cancel orders, so
// nothing rests in the book and no cancellation is ever needed. Strategies are
// deliberately cheap: a few arithmetic operations per symbol per tick.
package marketmaker

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"math"
	"math/rand/v2"
	"time"
)

// Ticker is a listed symbol and its reference price.
type Ticker struct {
	Symbol         string `json:"symbol"`
	ReferencePrice int64  `json:"reference_price"`
}

// Price is one symbol's current price.
type Price struct {
	Symbol string `json:"symbol"`
	Price  int64  `json:"price"`
}

// Snapshot is the market's prices as of one tick.
type Snapshot struct {
	Tick       uint64    `json:"tick"`
	NextTickAt time.Time `json:"next_tick_at"`
	Prices     []Price   `json:"prices"`
}

// Account is the maker's balances.
type Account struct {
	Cash     int64 `json:"cash"`
	CashHeld int64 `json:"cash_held"`
	Holdings []struct {
		Symbol   string `json:"symbol"`
		Quantity int64  `json:"quantity"`
		Held     int64  `json:"held"`
	} `json:"holdings"`
}

// Order is an order request in the public API's shape.
type Order struct {
	Symbol      string `json:"symbol"`
	Direction   string `json:"direction"`
	Type        string `json:"type"`
	Quantity    int64  `json:"quantity"`
	LimitPrice  int64  `json:"limit_price"`
	TimeInForce string `json:"time_in_force"`
}

func ioc(symbol, direction string, qty, price int64) Order {
	return Order{Symbol: symbol, Direction: direction, Type: "LIMIT", Quantity: qty, LimitPrice: price, TimeInForce: "IOC"}
}

// Client is the maker's view of the exchange.
type Client interface {
	Tickers(ctx context.Context) ([]Ticker, error)
	Prices(ctx context.Context) (Snapshot, error)
	Account(ctx context.Context) (Account, error)
	PlaceOrder(ctx context.Context, o Order) error
}

// Strategy decides a tick's orders. It may keep state between calls.
type Strategy interface {
	Orders(snap Snapshot, acct Account) []Order
}

// Config tunes the run loop. Zero values select defaults.
type Config struct {
	// Poll is how often to check for a new tick. Default 1s.
	Poll time.Duration
	// Delay is how long to wait after seeing a new tick before trading, so
	// the ledger has settled. Default 500ms.
	Delay  time.Duration
	Logger *slog.Logger
}

// Run trades once per tick until ctx is cancelled.
func Run(ctx context.Context, c Client, s Strategy, cfg Config) error {
	if cfg.Poll <= 0 {
		cfg.Poll = time.Second
	}
	if cfg.Delay <= 0 {
		cfg.Delay = 500 * time.Millisecond
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	log := cfg.Logger
	last := uint64(math.MaxUint64)
	t := time.NewTicker(cfg.Poll)
	defer t.Stop()
	for {
		if snap, err := c.Prices(ctx); err != nil {
			log.Warn("fetching prices", "err", err)
		} else if snap.Tick != last {
			last = snap.Tick
			if err := sleep(ctx, cfg.Delay); err != nil {
				return err
			}
			placed, rejected := trade(ctx, c, s, snap, log)
			log.Debug("traded", "tick", snap.Tick, "placed", placed, "rejected", rejected)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func trade(ctx context.Context, c Client, s Strategy, snap Snapshot, log *slog.Logger) (placed, rejected int) {
	acct, err := c.Account(ctx)
	if err != nil {
		log.Warn("fetching account", "err", err)
		return 0, 0
	}
	for _, o := range s.Orders(snap, acct) {
		if err := c.PlaceOrder(ctx, o); err != nil {
			rejected++
			var api *APIError
			if !errors.As(err, &api) || api.Status >= 500 {
				log.Warn("placing order", "symbol", o.Symbol, "err", err)
			}
			continue
		}
		placed++
	}
	return placed, rejected
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// book summarises an account for quick lookups.
type book struct {
	cash   int64
	shares map[string]int64
}

func available(a Account) book {
	b := book{cash: a.Cash - a.CashHeld, shares: make(map[string]int64, len(a.Holdings))}
	for _, h := range a.Holdings {
		b.shares[h.Symbol] = h.Quantity - h.Held
	}
	return b
}

// sizeFor converts a notional amount in cents to a share count, jittered by
// ±50% and at least one share.
func sizeFor(rng *rand.Rand, notional, price int64) int64 {
	q := float64(notional) / float64(price) * (0.5 + rng.Float64())
	return max(1, int64(q))
}

// Liquidity quotes a bid and an ask around each symbol's price. Its fair
// value reverts towards the reference price, and it skews quotes against its
// inventory so it never runs out of either side for long.
type Liquidity struct {
	Reference map[string]int64
	// HalfSpread is each quote's distance from fair value. Default 0.15%.
	HalfSpread float64
	// Reversion pulls fair value towards the reference each tick. Default 2%.
	Reversion float64
	// MaxSkew is the largest inventory skew of fair value. Default 0.5%.
	MaxSkew float64
	// Notional is the typical quote size in cents. Default $2,000.
	Notional int64

	rng    *rand.Rand
	target map[string]int64
}

func NewLiquidity(tickers []Ticker, seed uint64) *Liquidity {
	l := &Liquidity{Reference: map[string]int64{}, HalfSpread: 0.0015, Reversion: 0.02, MaxSkew: 0.005, Notional: 200_000,
		rng: rand.New(rand.NewPCG(seed, 1))}
	for _, t := range tickers {
		l.Reference[t.Symbol] = t.ReferencePrice
	}
	return l
}

func (l *Liquidity) Orders(snap Snapshot, acct Account) []Order {
	b := available(acct)
	if l.target == nil {
		// The first inventory seen is the level the maker steers back to.
		l.target = maps.Clone(b.shares)
	}
	var out []Order
	for _, p := range snap.Prices {
		ref, ok := l.Reference[p.Symbol]
		if !ok || p.Price <= 0 {
			continue
		}
		fair := float64(p.Price) + l.Reversion*float64(ref-p.Price)
		if target := l.target[p.Symbol]; target > 0 {
			excess := float64(b.shares[p.Symbol]-target) / float64(target)
			fair *= 1 - l.MaxSkew*math.Max(-1, math.Min(1, excess))
		}
		bid := int64(math.Floor(fair * (1 - l.HalfSpread)))
		ask := max(bid+1, int64(math.Ceil(fair*(1+l.HalfSpread))))

		if qty := min(sizeFor(l.rng, l.Notional, bid), b.cash/max(1, bid)); bid > 0 && qty > 0 {
			out = append(out, ioc(p.Symbol, "BUY", qty, bid))
			b.cash -= qty * bid
		}
		if qty := min(sizeFor(l.rng, l.Notional, ask), b.shares[p.Symbol]); qty > 0 {
			out = append(out, ioc(p.Symbol, "SELL", qty, ask))
		}
	}
	return out
}

// Flow is a noise trader. Each symbol has a sentiment that wanders randomly
// and decays towards neutral; each tick it crosses the spread on a random
// subset of symbols, buying more often when sentiment is positive.
type Flow struct {
	// Activity is the chance of trading a symbol each tick. Default 0.5.
	Activity float64
	// Aggression is how far past the price it is willing to trade. Default 0.3%.
	Aggression float64
	// Volatility scales sentiment shocks. Default 0.3.
	Volatility float64
	// Notional is the typical order size in cents. Default $500.
	Notional int64

	rng       *rand.Rand
	sentiment map[string]float64
}

func NewFlow(seed uint64) *Flow {
	return &Flow{Activity: 0.5, Aggression: 0.003, Volatility: 0.3, Notional: 50_000,
		rng: rand.New(rand.NewPCG(seed, 2)), sentiment: map[string]float64{}}
}

func (f *Flow) Orders(snap Snapshot, acct Account) []Order {
	b := available(acct)
	var out []Order
	for _, p := range snap.Prices {
		s := 0.9*f.sentiment[p.Symbol] + f.Volatility*f.rng.NormFloat64()
		s = math.Max(-1, math.Min(1, s))
		f.sentiment[p.Symbol] = s
		if p.Price <= 0 || f.rng.Float64() >= f.Activity {
			continue
		}
		qty := sizeFor(f.rng, f.Notional, p.Price)
		buy := f.rng.Float64() < (1+s)/2
		if limit := int64(math.Ceil(float64(p.Price) * (1 + f.Aggression))); buy {
			if qty = min(qty, b.cash/limit); qty > 0 {
				out = append(out, ioc(p.Symbol, "BUY", qty, limit))
				b.cash -= qty * limit
			}
		} else {
			limit := max(1, int64(math.Floor(float64(p.Price)*(1-f.Aggression))))
			if qty = min(qty, b.shares[p.Symbol]); qty > 0 {
				out = append(out, ioc(p.Symbol, "SELL", qty, limit))
			}
		}
	}
	return out
}
