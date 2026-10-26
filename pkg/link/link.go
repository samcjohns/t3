package link

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/samcjohns/t3/pkg/marketmaker"
)

// symbolState is what the maker remembers about one linked symbol. It is
// saved outside the mapping file, so copying in a new file never re-bases a
// link whose source is unchanged.
type symbolState struct {
	// Key is the link's source; a new one re-bases the symbol.
	Key string `json:"key"`
	// Signal is the latest outside value, fetched at FetchedAt. Fresh means
	// it has not yet been turned into a target.
	Signal    float64   `json:"signal"`
	FetchedAt time.Time `json:"fetched_at"`
	Fresh     bool      `json:"fresh"`
	// BaseSignal maps to BasePrice, in cents. Based is set once both are.
	BaseSignal float64 `json:"base_signal"`
	BasePrice  float64 `json:"base_price"`
	Based      bool    `json:"based"`
	// Target is the day's target price, and Anchor the price the maker
	// currently pulls towards, closing on Target over the link's half-life.
	// Both in cents; zero until known.
	Target float64 `json:"target"`
	Anchor float64 `json:"anchor"`
}

// Maker is the link strategy. Watch fetches outside values in the
// background; Orders trades on them each tick. It is safe for both to run
// at once.
type Maker struct {
	// Now is the clock. Default time.Now.
	Now func() time.Time

	path, statePath string
	src             Source
	log             *slog.Logger
	rng             *rand.Rand

	mu       sync.Mutex
	cfg      config
	state    map[string]*symbolState
	attempts map[string]time.Time // last failed fetch, by symbol
	last     time.Time            // previous call to Orders
	saved    time.Time
}

// stateSaveEvery is how often Orders saves anchors' progress.
const stateSaveEvery = 5 * time.Minute

// retryAfter is how long a failed fetch waits before trying again.
const retryAfter = time.Hour

// New loads the mapping file at path and the state at statePath. A missing
// or bad mapping file is logged, and the maker idles until a good one
// appears; a missing state file starts afresh.
func New(path, statePath string, src Source, seed uint64, log *slog.Logger) (*Maker, error) {
	m := &Maker{Now: time.Now, path: path, statePath: statePath, src: src, log: log,
		rng: rand.New(rand.NewPCG(seed, 5)), state: map[string]*symbolState{}, attempts: map[string]time.Time{}}
	m.cfg.settings = Settings{}.withDefaults()
	b, err := os.ReadFile(statePath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, &m.state); err != nil {
			return nil, err
		}
	}
	m.reload()
	return m, nil
}

// reload re-reads the mapping file, keeping the last good config, or the
// last good link for a symbol, where the new one is bad.
func (m *Maker) reload() {
	cfg, skipped, err := load(m.path)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.log.Warn("link file unusable; keeping the last good one", "err", err)
		return
	}
	for name, err := range skipped {
		if old, ok := m.cfg.links[name]; ok {
			cfg.links[name] = old
		}
		m.log.Warn("link entry unusable", "symbol", name, "err", err)
	}
	if !reflect.DeepEqual(cfg, m.cfg) {
		m.log.Info("links loaded", "links", len(cfg.links))
	}
	m.cfg = cfg
}

// Watch re-reads the mapping file and fetches due values every interval
// until ctx is cancelled. A link is due once a day at the configured time,
// at once when new, and an hour after a failed fetch.
func (m *Maker) Watch(ctx context.Context, interval time.Duration) error {
	for {
		m.reload()
		m.fetchDue(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func (m *Maker) fetchDue(ctx context.Context) {
	now := m.Now()
	m.mu.Lock()
	slot := m.cfg.settings.lastFetch(now)
	var due []Link
	for sym, l := range m.cfg.links {
		st := m.state[sym]
		if st != nil && st.Key == l.key() && !st.FetchedAt.Before(slot) {
			continue
		}
		if now.Sub(m.attempts[sym]) < retryAfter {
			continue
		}
		due = append(due, l)
	}
	m.mu.Unlock()

	for _, l := range due {
		v, err := m.src.Fetch(ctx, l)
		if err == nil && !usable(v, l.Mode) {
			err = errors.New("unusable value")
		}
		m.mu.Lock()
		if err != nil {
			m.attempts[l.Symbol] = now
			m.log.Warn("link fetch failed", "symbol", l.Symbol, "source", l.Source, "err", err)
		} else {
			delete(m.attempts, l.Symbol)
			st := m.state[l.Symbol]
			if st == nil || st.Key != l.key() {
				st = &symbolState{Key: l.key()}
				m.state[l.Symbol] = st
			}
			st.Signal, st.FetchedAt, st.Fresh = v, now, true
			m.log.Info("link fetched", "symbol", l.Symbol, "key", st.Key, "signal", v)
		}
		m.mu.Unlock()
	}
	if len(due) > 0 {
		m.save()
	}
}

// Orders trades each linked symbol gently towards its anchor and quotes deep
// walls either side of the link's corridor. Unlinked symbols, and links not
// yet fetched, are left alone.
func (m *Maker) Orders(snap marketmaker.Snapshot, acct marketmaker.Account) []marketmaker.Order {
	now := m.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	// Time the maker was down doesn't count, so a restart never jumps.
	dt := 0.0
	if !m.last.IsZero() {
		dt = min(now.Sub(m.last), time.Minute).Seconds()
	}
	m.last = now

	s := m.cfg.settings
	cash, shares := available(acct)
	var out []marketmaker.Order
	for _, p := range snap.Prices {
		l, linked := m.cfg.links[p.Symbol]
		st := m.state[p.Symbol]
		if !linked || st == nil || st.Key != l.key() || p.Price <= 0 {
			continue
		}
		last := float64(p.Price)
		m.retarget(l, st, last)
		if st.Target <= 0 {
			continue
		}
		st.Anchor += (1 - math.Pow(0.5, dt/(l.HalfLifeDays*86400))) * (st.Target - st.Anchor)

		if o, ok := pull(p.Symbol, last, st.Anchor, s, &cash, shares); ok {
			out = append(out, o)
		}
		bid := math.Min(st.Anchor*(1-s.Band), last*(1+s.MaxLean))
		ask := math.Max(st.Anchor*(1+s.Band), last*(1-s.MaxLean))
		out = append(out, walls(m.rng, p.Symbol, bid, ask, s.Depth, &cash, shares)...)
	}
	if now.Sub(m.saved) >= stateSaveEvery {
		m.saveLocked()
	}
	return out
}

// retarget bases a newly linked symbol at the current price, and turns a
// fresh signal into the day's target.
func (m *Maker) retarget(l Link, st *symbolState, last float64) {
	if !st.Based {
		if !st.Fresh {
			return
		}
		st.BaseSignal, st.BasePrice, st.Based = st.Signal, last, true
		if l.BasePrice > 0 {
			st.BasePrice = float64(l.BasePrice)
		}
		st.Anchor = last
		m.log.Info("link based", "symbol", l.Symbol, "key", st.Key, "base_signal", st.BaseSignal, "base_price", st.BasePrice)
	}
	if !st.Fresh {
		return
	}
	st.Fresh = false
	var raw float64
	if l.Mode == Ratio {
		raw = st.BasePrice * math.Pow(st.Signal/st.BaseSignal, l.Sensitivity)
	} else {
		raw = st.BasePrice * (1 + l.Sensitivity*(st.Signal-st.BaseSignal))
	}
	prev := st.Target
	if prev <= 0 {
		prev = st.BasePrice
	}
	move := m.cfg.settings.MaxDailyMove
	st.Target = math.Max(1, math.Max(prev*(1-move), math.Min(prev*(1+move), raw)))
	if math.IsNaN(st.Target) {
		st.Target = prev
	}
	if st.Anchor <= 0 {
		st.Anchor = last
	}
	m.log.Info("link target", "symbol", l.Symbol, "signal", st.Signal, "target", st.Target, "uncapped", raw, "price", last)
}

// pullAggression is how far past the last price a pull order trades.
const pullAggression = 0.003

// pull returns an IOC order towards anchor, sized by how far the price is
// from it as a fraction of the band, or false if there is nothing to trade.
func pull(symbol string, last, anchor float64, s Settings, cash *int64, shares map[string]int64) (marketmaker.Order, bool) {
	if s.Band <= 0 {
		return marketmaker.Order{}, false
	}
	dev := (anchor - last) / anchor
	notional := float64(s.Pull) * math.Min(1, math.Abs(dev)/s.Band)
	if dev > 0 {
		limit := int64(math.Ceil(last * (1 + pullAggression)))
		if qty := min(int64(notional)/limit, *cash/limit); qty > 0 {
			*cash -= qty * limit
			return ioc(symbol, "BUY", qty, limit), true
		}
		return marketmaker.Order{}, false
	}
	limit := max(1, int64(math.Floor(last*(1-pullAggression))))
	if qty := min(int64(notional)/limit, shares[symbol]); qty > 0 {
		shares[symbol] -= qty
		return ioc(symbol, "SELL", qty, limit), true
	}
	return marketmaker.Order{}, false
}

// walls quotes depth on each side, as a ladder of bids down from bidAt and
// asks up from askAt, within the cash and shares available, which it spends.
func walls(rng *rand.Rand, symbol string, bidAt, askAt float64, depth int64, cash *int64, shares map[string]int64) []marketmaker.Order {
	var weights float64
	for _, lv := range marketmaker.DefaultLevels {
		weights += lv.Weight
	}
	var out []marketmaker.Order
	bid, ask := int64(math.MaxInt64), int64(0)
	for _, lv := range marketmaker.DefaultLevels {
		notional := float64(depth) * lv.Weight / weights * (0.8 + 0.4*rng.Float64())
		bid = min(bid-1, int64(math.Floor(bidAt*(1-lv.Offset))))
		ask = max(ask+1, int64(math.Ceil(askAt*(1+lv.Offset))))
		if qty := min(max(1, int64(notional)/max(1, bid)), *cash/max(1, bid)); bid > 0 && qty > 0 {
			out = append(out, ioc(symbol, "BUY", qty, bid))
			*cash -= qty * bid
		}
		if qty := min(max(1, int64(notional)/ask), shares[symbol]); qty > 0 {
			out = append(out, ioc(symbol, "SELL", qty, ask))
			shares[symbol] -= qty
		}
	}
	return out
}

func ioc(symbol, direction string, qty, price int64) marketmaker.Order {
	return marketmaker.Order{Symbol: symbol, Direction: direction, Type: "LIMIT", Quantity: qty, LimitPrice: price, TimeInForce: "IOC"}
}

func available(a marketmaker.Account) (int64, map[string]int64) {
	shares := make(map[string]int64, len(a.Holdings))
	for _, h := range a.Holdings {
		shares[h.Symbol] = h.Quantity - h.Held
	}
	return a.Cash - a.CashHeld, shares
}

// Save writes the state now, as on shutdown.
func (m *Maker) Save() { m.save() }

func (m *Maker) save() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saveLocked()
}

// saveLocked writes the state atomically. A failure is logged and retried
// at the next save.
func (m *Maker) saveLocked() {
	m.saved = m.Now()
	b, err := json.MarshalIndent(m.state, "", "  ")
	if err == nil {
		tmp := m.statePath + ".tmp"
		if err = os.WriteFile(tmp, b, 0o600); err == nil {
			err = os.Rename(tmp, m.statePath)
		}
	}
	if err != nil {
		m.log.Warn("saving link state", "dir", filepath.Dir(m.statePath), "err", err)
	}
}
