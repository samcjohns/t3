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
	LimitPrice  int64  `json:"limit_price,omitempty"`
	MaxCost     int64  `json:"max_cost,omitempty"`
	TimeInForce string `json:"time_in_force,omitempty"`
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
// ±jitter and at least one share.
func sizeFor(rng *rand.Rand, notional, price int64, jitter float64) int64 {
	q := float64(notional) / float64(price) * (1 - jitter + 2*jitter*rng.Float64())
	return max(1, int64(q))
}

// Level is one rung of a quote ladder.
type Level struct {
	// Offset is the quote's distance from fair value, as a fraction.
	Offset float64
	// Weight is the level's share of the side's depth, relative to the
	// other levels.
	Weight float64
}

// DefaultLevels put a sixth of the depth within 0.3% of fair value, so a new
// player's whole account fills close to the last price, and the rest further
// out, so a much larger order still fills at a worse price instead of not at
// all.
var DefaultLevels = []Level{{0.0015, 1}, {0.003, 2}, {0.006, 3}, {0.012, 5}, {0.025, 9}}

// Liquidity quotes a ladder of bids and asks around each symbol's price. Its
// fair value reverts towards the reference price, and it skews quotes against
// its inventory so it never runs out of either side for long.
type Liquidity struct {
	Reference map[string]int64
	// Levels is the ladder quoted on each side. Default DefaultLevels.
	Levels []Level
	// Depth is the total notional quoted on each side of each symbol, in
	// cents. Default $100,000.
	Depth int64
	// Reversion pulls fair value towards the reference each tick. Default 2%.
	Reversion float64
	// MaxSkew is the largest inventory skew of fair value. Default 0.5%.
	MaxSkew float64
	// MaxLean caps how far fair value sits from the last price, so the
	// inner levels stay where players expect to trade. Default 0.5%.
	MaxLean float64

	rng    *rand.Rand
	target map[string]int64
}

func NewLiquidity(tickers []Ticker, seed uint64) *Liquidity {
	l := &Liquidity{Reference: map[string]int64{}, Levels: DefaultLevels, Depth: 10_000_000,
		Reversion: 0.02, MaxSkew: 0.005, MaxLean: 0.005, rng: rand.New(rand.NewPCG(seed, 1))}
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
	var weights float64
	for _, lv := range l.Levels {
		weights += lv.Weight
	}
	var out []Order
	for _, p := range snap.Prices {
		ref, ok := l.Reference[p.Symbol]
		if !ok || p.Price <= 0 {
			continue
		}
		last := float64(p.Price)
		fair := last + l.Reversion*(float64(ref)-last)
		if target := l.target[p.Symbol]; target > 0 {
			excess := float64(b.shares[p.Symbol]-target) / float64(target)
			fair *= 1 - l.MaxSkew*math.Max(-1, math.Min(1, excess))
		}
		fair = math.Max(last*(1-l.MaxLean), math.Min(last*(1+l.MaxLean), fair))

		// Each level is strictly outside the one before it, even where
		// rounding to whole cents would merge them.
		bid, ask := int64(math.MaxInt64), int64(0)
		for _, lv := range l.Levels {
			notional := int64(float64(l.Depth) * lv.Weight / weights)
			bid = min(bid-1, int64(math.Floor(fair*(1-lv.Offset))))
			ask = max(ask+1, int64(math.Ceil(fair*(1+lv.Offset))))
			if qty := min(sizeFor(l.rng, notional, max(1, bid), 0.2), b.cash/max(1, bid)); bid > 0 && qty > 0 {
				out = append(out, ioc(p.Symbol, "BUY", qty, bid))
				b.cash -= qty * bid
			}
			if qty := min(sizeFor(l.rng, notional, ask, 0.2), b.shares[p.Symbol]); qty > 0 {
				out = append(out, ioc(p.Symbol, "SELL", qty, ask))
				b.shares[p.Symbol] -= qty
			}
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
		qty := sizeFor(f.rng, f.Notional, p.Price, 0.5)
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

// deviation is how far price sits from ref, as a fraction of ref.
func deviation(price, ref int64) float64 {
	return float64(price-ref) / float64(ref)
}

// cross returns an IOC order crossing the spread by aggression, sized to
// notional and capped by available funds, or false if it cannot be funded.
func cross(rng *rand.Rand, b *book, symbol string, price int64, buy bool, notional int64, aggression float64) (Order, bool) {
	qty := sizeFor(rng, notional, price, 0.3)
	if buy {
		limit := int64(math.Ceil(float64(price) * (1 + aggression)))
		if qty = min(qty, b.cash/limit); qty <= 0 {
			return Order{}, false
		}
		b.cash -= qty * limit
		return ioc(symbol, "BUY", qty, limit), true
	}
	limit := max(1, int64(math.Floor(float64(price)*(1-aggression))))
	if qty = min(qty, b.shares[symbol]); qty <= 0 {
		return Order{}, false
	}
	b.shares[symbol] -= qty
	return ioc(symbol, "SELL", qty, limit), true
}

// Momentum is a trend follower. It tracks a fast and a slow moving average
// of each symbol's price and trades in the direction of the gap between
// them, sized by its strength, so moves that start tend to run for a while
// before the liquidity maker's reversion wins. It stops chasing a trend once
// the price is MaxDeviation from its reference.
type Momentum struct {
	Reference map[string]int64
	// Fast and Slow are the moving averages' smoothing factors per tick.
	// Default 0.3 and 0.05.
	Fast, Slow float64
	// Threshold is the smallest gap between the averages, as a fraction of
	// price, that it trades on. Default 0.1%.
	Threshold float64
	// Saturation is the gap at which it trades its full Notional. Default 1%.
	Saturation float64
	// Activity is the chance of trading a signalled symbol each tick.
	// Default 0.7.
	Activity float64
	// Aggression is how far past the price it is willing to trade. Default 0.4%.
	Aggression float64
	// Notional is the largest order size in cents. Default $5,000.
	Notional int64
	// MaxDeviation is how far from its reference it lets a trend carry a
	// price. Default 10%.
	MaxDeviation float64

	rng        *rand.Rand
	fast, slow map[string]float64
}

func NewMomentum(tickers []Ticker, seed uint64) *Momentum {
	m := &Momentum{Reference: map[string]int64{}, Fast: 0.3, Slow: 0.05, Threshold: 0.001, Saturation: 0.01,
		Activity: 0.7, Aggression: 0.004, Notional: 500_000, MaxDeviation: 0.1,
		rng: rand.New(rand.NewPCG(seed, 3)), fast: map[string]float64{}, slow: map[string]float64{}}
	for _, t := range tickers {
		m.Reference[t.Symbol] = t.ReferencePrice
	}
	return m
}

func (m *Momentum) Orders(snap Snapshot, acct Account) []Order {
	b := available(acct)
	var out []Order
	for _, p := range snap.Prices {
		ref, ok := m.Reference[p.Symbol]
		if !ok || p.Price <= 0 {
			continue
		}
		price := float64(p.Price)
		fast, seen := m.fast[p.Symbol]
		if !seen {
			m.fast[p.Symbol], m.slow[p.Symbol] = price, price
			continue
		}
		fast += m.Fast * (price - fast)
		slow := m.slow[p.Symbol] + m.Slow*(price-m.slow[p.Symbol])
		m.fast[p.Symbol], m.slow[p.Symbol] = fast, slow

		signal := (fast - slow) / slow
		if math.Abs(signal) < m.Threshold || m.rng.Float64() >= m.Activity {
			continue
		}
		buy := signal > 0
		if dev := deviation(p.Price, ref); buy && dev > m.MaxDeviation || !buy && dev < -m.MaxDeviation {
			continue
		}
		notional := int64(float64(m.Notional) * math.Min(1, math.Abs(signal)/m.Saturation))
		if o, ok := cross(m.rng, &b, p.Symbol, p.Price, buy, notional, m.Aggression); ok {
			out = append(out, o)
		}
	}
	return out
}

// News trades on imaginary headlines. Now and then a symbol gets news that
// sets a target a few percent away, and it trades hard towards the target
// for a few ticks, giving the price a sudden jump the other makers then
// absorb. News is more likely to push a price back towards its reference
// than further away, and never sets a target beyond MaxDeviation from it.
type News struct {
	Reference map[string]int64
	// Rate is the chance of news per symbol per tick. Default 1%.
	Rate float64
	// MinImpact and MaxImpact bound the size of a target move. Default 2% and 8%.
	MinImpact, MaxImpact float64
	// MinTicks and MaxTicks bound how long it trades on one headline.
	// Default 3 and 10.
	MinTicks, MaxTicks int
	// Aggression is how far past the price it is willing to trade, though
	// never past the target. Default 0.6%, inside the 1% players are told
	// fills in full, so a player's order there has priority over news.
	Aggression float64
	// Notional is the typical order size in cents. Default $20,000.
	Notional int64
	// MaxDeviation is the furthest from its reference a target can be.
	// Default 20%.
	MaxDeviation float64

	rng     *rand.Rand
	stories map[string]story
}

type story struct {
	target int64
	ticks  int
}

func NewNews(tickers []Ticker, seed uint64) *News {
	n := &News{Reference: map[string]int64{}, Rate: 0.01, MinImpact: 0.02, MaxImpact: 0.08,
		MinTicks: 3, MaxTicks: 10, Aggression: 0.006, Notional: 2_000_000, MaxDeviation: 0.2,
		rng: rand.New(rand.NewPCG(seed, 4)), stories: map[string]story{}}
	for _, t := range tickers {
		n.Reference[t.Symbol] = t.ReferencePrice
	}
	return n
}

func (n *News) Orders(snap Snapshot, acct Account) []Order {
	b := available(acct)
	var out []Order
	for _, p := range snap.Prices {
		ref, ok := n.Reference[p.Symbol]
		if !ok || p.Price <= 0 {
			continue
		}
		s, active := n.stories[p.Symbol]
		if !active {
			if n.rng.Float64() >= n.Rate {
				continue
			}
			s = n.headline(p.Price, ref)
		}
		if s.ticks--; s.ticks <= 0 || s.target == p.Price {
			delete(n.stories, p.Symbol)
		} else {
			n.stories[p.Symbol] = s
		}
		if s.target == p.Price {
			continue
		}

		buy := s.target > p.Price
		gap := math.Abs(deviation(s.target, p.Price))
		if o, ok := cross(n.rng, &b, p.Symbol, p.Price, buy, n.Notional, math.Min(n.Aggression, gap)); ok {
			out = append(out, o)
		}
	}
	return out
}

// headline draws a new story for a symbol. Good news is likelier the further
// the price is below its reference.
func (n *News) headline(price, ref int64) story {
	dev := math.Max(-1, math.Min(1, deviation(price, ref)/n.MaxDeviation))
	impact := n.MinImpact + (n.MaxImpact-n.MinImpact)*n.rng.Float64()
	if n.rng.Float64() >= (1-dev)/2 {
		impact = -impact
	}
	lo, hi := float64(ref)*(1-n.MaxDeviation), float64(ref)*(1+n.MaxDeviation)
	target := math.Max(lo, math.Min(hi, float64(price)*(1+impact)))
	return story{target: max(1, int64(math.Round(target))), ticks: n.MinTicks + n.rng.IntN(n.MaxTicks-n.MinTicks+1)}
}
