// Package reporting is the read-only reporting service: it ingests the market
// engine's tick results into query-friendly views (trade tape, quotes, candles,
// per-account trade history) and combines them with ledger balances for
// portfolio valuation. It never changes market or ledger state.
package reporting

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/samcjohns/t3/pkg/engine"
	"github.com/samcjohns/t3/pkg/ledger"
)

// CandleBase is the resolution candles are stored at. Coarser intervals are
// aggregated from it on demand.
const CandleBase = time.Minute

var (
	ErrUnknownSymbol   = errors.New("unknown symbol")
	ErrInvalidInterval = errors.New("invalid candle interval")
)

// Trade is one execution as seen publicly, without account or order IDs.
type Trade struct {
	Tick     uint64    `json:"tick"`
	Time     time.Time `json:"time"`
	Symbol   string    `json:"symbol"`
	Price    int64     `json:"price"`
	Quantity int64     `json:"quantity"`
}

// AccountTrade is one side of an execution, as seen by the account on it.
type AccountTrade struct {
	Tick      uint64           `json:"tick"`
	Time      time.Time        `json:"time"`
	Symbol    string           `json:"symbol"`
	OrderID   string           `json:"order_id"`
	Direction engine.Direction `json:"direction"`
	Price     int64            `json:"price"`
	Quantity  int64            `json:"quantity"`
}

// Quote is the latest clearing result for a symbol that has traded.
type Quote struct {
	Symbol    string    `json:"symbol"`
	LastPrice int64     `json:"last_price"`
	LastTick  uint64    `json:"last_tick"`
	LastTime  time.Time `json:"last_time"`
}

// Candle summarises clearing prices over [Start, Start+interval).
type Candle struct {
	Start  time.Time `json:"start"`
	Open   int64     `json:"open"`
	High   int64     `json:"high"`
	Low    int64     `json:"low"`
	Close  int64     `json:"close"`
	Volume int64     `json:"volume"`
}

// Position is one holding valued at its symbol's last price. LastPrice and
// Value are 0 if the symbol has never traded.
type Position struct {
	Symbol    string `json:"symbol"`
	Quantity  int64  `json:"quantity"`
	Held      int64  `json:"held"`
	LastPrice int64  `json:"last_price"`
	Value     int64  `json:"value"`
}

// Portfolio is an account's balances valued at last prices.
type Portfolio struct {
	AccountID   string     `json:"account_id"`
	Cash        int64      `json:"cash"`
	CashHeld    int64      `json:"cash_held"`
	Positions   []Position `json:"positions"`
	MarketValue int64      `json:"market_value"`
	TotalValue  int64      `json:"total_value"`
}

// AccountReader is the read side of the ledger that portfolios need.
type AccountReader interface {
	Account(id string) (ledger.Account, error)
}

// Config bounds memory use. Zero values select defaults.
type Config struct {
	// MaxTradesPerSymbol caps the public tape kept per symbol. Default 1000.
	MaxTradesPerSymbol int
	// MaxTradesPerAccount caps the history kept per account. Default 1000.
	MaxTradesPerAccount int
}

// Service is safe for concurrent use.
type Service struct {
	accounts           AccountReader
	maxSymbol, maxAcct int
	mu                 sync.RWMutex
	lastTick           uint64
	quotes             map[string]Quote
	trades             map[string][]Trade
	accountTrades      map[string][]AccountTrade
	candles            map[string][]Candle
}

func New(accounts AccountReader, cfg Config) *Service {
	if cfg.MaxTradesPerSymbol <= 0 {
		cfg.MaxTradesPerSymbol = 1000
	}
	if cfg.MaxTradesPerAccount <= 0 {
		cfg.MaxTradesPerAccount = 1000
	}
	return &Service{
		accounts:      accounts,
		maxSymbol:     cfg.MaxTradesPerSymbol,
		maxAcct:       cfg.MaxTradesPerAccount,
		quotes:        make(map[string]Quote),
		trades:        make(map[string][]Trade),
		accountTrades: make(map[string][]AccountTrade),
		candles:       make(map[string][]Candle),
	}
}

// Ingest adds a tick's results to the views. Ticks at or before the last
// ingested tick are ignored, so redelivery is harmless. Gaps are tolerated:
// the service can start after the engine and simply has less history.
func (s *Service) Ingest(tr engine.TickResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tr.Tick <= s.lastTick {
		return
	}
	s.lastTick = tr.Tick

	for _, book := range tr.Books {
		if book.Volume == 0 {
			continue
		}
		s.quotes[book.Symbol] = Quote{Symbol: book.Symbol, LastPrice: book.ClearingPrice, LastTick: tr.Tick, LastTime: tr.Timestamp}
		s.addCandle(book.Symbol, tr.Timestamp, book.ClearingPrice, book.Volume)

		for _, e := range book.Executions {
			s.trades[e.Symbol] = appendCapped(s.trades[e.Symbol], s.maxSymbol,
				Trade{Tick: tr.Tick, Time: tr.Timestamp, Symbol: e.Symbol, Price: e.Price, Quantity: e.Quantity})
			for _, side := range []struct {
				account, order string
				dir            engine.Direction
			}{{e.BuyAccountID, e.BuyOrderID, engine.Buy}, {e.SellAccountID, e.SellOrderID, engine.Sell}} {
				s.accountTrades[side.account] = appendCapped(s.accountTrades[side.account], s.maxAcct, AccountTrade{
					Tick: tr.Tick, Time: tr.Timestamp, Symbol: e.Symbol, OrderID: side.order,
					Direction: side.dir, Price: e.Price, Quantity: e.Quantity,
				})
			}
		}
	}
}

func (s *Service) addCandle(symbol string, at time.Time, price, volume int64) {
	start := at.UTC().Truncate(CandleBase)
	cs := s.candles[symbol]
	if n := len(cs); n > 0 && !start.After(cs[n-1].Start) {
		// Same minute, or a clock step backwards: fold into the latest candle
		// so candles stay strictly ordered.
		c := &cs[n-1]
		c.High, c.Low, c.Close = max(c.High, price), min(c.Low, price), price
		c.Volume += volume
		return
	}
	s.candles[symbol] = append(cs, Candle{Start: start, Open: price, High: price, Low: price, Close: price, Volume: volume})
}

// appendCapped appends v and keeps at most limit of the newest entries,
// compacting only occasionally so appends stay amortised O(1).
func appendCapped[T any](s []T, limit int, v T) []T {
	s = append(s, v)
	if len(s) > 2*limit {
		s = slices.Clone(s[len(s)-limit:])
	}
	return s
}

// newest returns up to limit of the newest entries, newest first, honouring
// the configured cap. limit <= 0 means all retained entries.
func newest[T any](s []T, limit, retained int) []T {
	if len(s) > retained {
		s = s[len(s)-retained:]
	}
	if limit > 0 && len(s) > limit {
		s = s[len(s)-limit:]
	}
	out := slices.Clone(s)
	slices.Reverse(out)
	if out == nil {
		out = []T{}
	}
	return out
}

func (s *Service) Quote(symbol string) (Quote, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	q, ok := s.quotes[symbol]
	if !ok {
		return Quote{}, fmt.Errorf("%w: %q has not traded", ErrUnknownSymbol, symbol)
	}
	return q, nil
}

// Trades returns a symbol's most recent trades, newest first.
func (s *Service) Trades(symbol string, limit int) []Trade {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return newest(s.trades[symbol], limit, s.maxSymbol)
}

// AccountTrades returns an account's most recent fills, newest first.
func (s *Service) AccountTrades(accountID string, limit int) []AccountTrade {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return newest(s.accountTrades[accountID], limit, s.maxAcct)
}

// Candles returns up to limit of a symbol's most recent candles at interval,
// oldest first. Interval must be a positive multiple of CandleBase; buckets
// are aligned to the Unix epoch in UTC. Only periods with trades appear.
func (s *Service) Candles(symbol string, interval time.Duration, limit int) ([]Candle, error) {
	if interval <= 0 || interval%CandleBase != 0 {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInterval, interval)
	}
	s.mu.RLock()
	base := slices.Clone(s.candles[symbol])
	s.mu.RUnlock()

	out := []Candle{}
	for _, c := range base {
		start := c.Start.Truncate(interval)
		if n := len(out); n > 0 && out[n-1].Start.Equal(start) {
			agg := &out[n-1]
			agg.High, agg.Low, agg.Close = max(agg.High, c.High), min(agg.Low, c.Low), c.Close
			agg.Volume += c.Volume
			continue
		}
		c.Start = start
		out = append(out, c)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

// Portfolio values an account's ledger balances at last prices.
func (s *Service) Portfolio(accountID string) (Portfolio, error) {
	acct, err := s.accounts.Account(accountID)
	if err != nil {
		return Portfolio{}, err
	}
	s.mu.RLock()
	quotes := maps.Clone(s.quotes)
	s.mu.RUnlock()

	p := Portfolio{AccountID: acct.ID, Cash: acct.Cash, CashHeld: acct.CashHeld, Positions: []Position{}}
	for _, h := range acct.Holdings {
		last := quotes[h.Symbol].LastPrice
		pos := Position{Symbol: h.Symbol, Quantity: h.Quantity, Held: h.Held, LastPrice: last, Value: last * h.Quantity}
		p.Positions = append(p.Positions, pos)
		p.MarketValue += pos.Value
	}
	p.TotalValue = p.Cash + p.MarketValue
	return p, nil
}
