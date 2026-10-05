package marketmaker

import (
	"context"
	"log/slog"
	"math"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samcjohns/t3/pkg/engine"
	"github.com/samcjohns/t3/pkg/gateway"
	"github.com/samcjohns/t3/pkg/ledger"
	"github.com/samcjohns/t3/pkg/listing"
	"github.com/samcjohns/t3/pkg/reporting"
)

var ctx = context.Background()

func TestLiquidityQuotesWithinFunds(t *testing.T) {
	l := NewLiquidity([]Ticker{{"ACME", 1000}, {"ZED", 500}}, 1)
	acct := Account{Cash: 100_000, Holdings: []struct {
		Symbol   string `json:"symbol"`
		Quantity int64  `json:"quantity"`
		Held     int64  `json:"held"`
	}{{"ACME", 3, 0}}}
	snap := Snapshot{Prices: []Price{{"ACME", 1000}, {"ZED", 500}, {"UNLISTED", 10}}}

	var spent int64
	bids := map[string]int64{}
	sold := map[string]int64{}
	for _, o := range l.Orders(snap, acct) {
		if o.TimeInForce != "IOC" || o.Symbol == "UNLISTED" {
			t.Fatalf("bad order %+v", o)
		}
		if o.Direction == "BUY" {
			spent += o.Quantity * o.LimitPrice
			bids[o.Symbol] = max(bids[o.Symbol], o.LimitPrice)
		} else {
			if sold[o.Symbol] += o.Quantity; o.Symbol != "ACME" || sold[o.Symbol] > 3 {
				t.Fatalf("sell beyond holdings: %+v", o)
			}
			if o.LimitPrice <= bids["ACME"] {
				t.Fatalf("ask %d not above bid %d", o.LimitPrice, bids["ACME"])
			}
		}
	}
	if spent > 100_000 {
		t.Fatalf("bids need %d cash, have 100000", spent)
	}
}

func TestFlowRespectsFunds(t *testing.T) {
	f := NewFlow(1)
	f.Activity = 1
	snap := Snapshot{Prices: []Price{{"ACME", 1000}, {"ZED", 500}}}
	for range 50 {
		for _, o := range f.Orders(snap, Account{}) {
			t.Fatalf("order with no funds: %+v", o)
		}
	}
}

var makers = []string{"mm-liquidity", "mm-flow", "mm-momentum", "mm-news"}

func TestMomentumFollowsTheTrend(t *testing.T) {
	m := NewMomentum([]Ticker{{"ACME", 1000}}, 1)
	m.Activity, m.MaxDeviation = 1, 0.2
	acct := Account{Cash: 10_000_000, Holdings: []struct {
		Symbol   string `json:"symbol"`
		Quantity int64  `json:"quantity"`
		Held     int64  `json:"held"`
	}{{"ACME", 100_000, 0}}}
	dirs := func(price int64) map[string]int {
		n := map[string]int{}
		for _, o := range m.Orders(Snapshot{Prices: []Price{{"ACME", price}}}, acct) {
			n[o.Direction]++
		}
		return n
	}
	for price := int64(1000); price <= 1100; price += 10 {
		if d := dirs(price); d["SELL"] > 0 {
			t.Fatalf("sold into a rally at %d", price)
		}
	}
	if d := dirs(1110); d["BUY"] != 1 {
		t.Fatalf("not buying a rally: %v", d)
	}
	// Beyond MaxDeviation it stops chasing.
	for price := int64(1150); price <= 1300; price += 25 {
		if d := dirs(price); price > 1200 && d["BUY"] > 0 {
			t.Fatalf("bought at %d, beyond MaxDeviation", price)
		}
	}
	// Once the averages cross on the way down, it sells.
	var sold int
	for price := int64(1300); price >= 900; price -= 20 {
		d := dirs(price)
		if price < 1100 && d["BUY"] > 0 {
			t.Fatalf("bought into a slide at %d", price)
		}
		sold += d["SELL"]
	}
	if sold == 0 {
		t.Fatal("never sold a slide")
	}
}

func TestNewsTargetsStayBounded(t *testing.T) {
	n := NewNews([]Ticker{{"ACME", 1000}}, 1)
	n.Rate, n.MaxDeviation = 1, 0.3
	acct := Account{Cash: 1_000_000_000, Holdings: []struct {
		Symbol   string `json:"symbol"`
		Quantity int64  `json:"quantity"`
		Held     int64  `json:"held"`
	}{{"ACME", 1_000_000, 0}}}
	var ups, downs int
	for i := range 2000 {
		price := int64(700 + i%601)
		for _, o := range n.Orders(Snapshot{Prices: []Price{{"ACME", price}}}, acct) {
			if o.TimeInForce != "IOC" {
				t.Fatalf("bad order %+v", o)
			}
			if o.Direction == "BUY" {
				ups++
				if o.LimitPrice > 1300 {
					t.Fatalf("bought at %d, beyond MaxDeviation", o.LimitPrice)
				}
			} else {
				downs++
				if o.LimitPrice < 700 {
					t.Fatalf("sold at %d, beyond MaxDeviation", o.LimitPrice)
				}
			}
		}
		if s, ok := n.stories["ACME"]; ok && (s.target < 700 || s.target > 1300) {
			t.Fatalf("target %d beyond MaxDeviation", s.target)
		}
	}
	if ups == 0 || downs == 0 {
		t.Fatalf("one-sided news: %d up, %d down", ups, downs)
	}
	for range 50 {
		for _, o := range n.Orders(Snapshot{Prices: []Price{{"ACME", 1000}}}, Account{}) {
			t.Fatalf("order with no funds: %+v", o)
		}
	}
}

// exchange is a real in-process exchange, served over HTTP, with every maker
// seeded as in production.
type exchange struct {
	gw          *gateway.Gateway
	eng         *engine.Orchestrator
	url         string
	totalCash   int64
	totalShares map[string]int64
}

func newExchange(t *testing.T) *exchange {
	tickers := listing.Default()
	ldg := ledger.New()
	reports := reporting.New(ldg, reporting.Config{Listing: tickers, Heartbeat: 10 * time.Second})
	eng := engine.NewOrchestrator(engine.FBAMatcher{}, engine.Config{OnTick: func(tr engine.TickResult) {
		if err := ldg.ApplyTick(ctx, tr); err != nil {
			t.Errorf("ApplyTick: %v", err)
		}
		reports.Ingest(tr)
	}})
	gw := gateway.New(eng, ldg, reports, gateway.Config{Tickers: tickers, StartingCash: 1_000_000, PasswordIterations: 1, RateLimit: 1e6, RateBurst: 1e6, Logger: slog.New(slog.DiscardHandler)})
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)

	x := &exchange{gw: gw, eng: eng, url: srv.URL, totalShares: map[string]int64{}}
	for _, name := range makers {
		u, err := gw.EnsureUser(ctx, name, "maker password", gateway.RoleMarketMaker)
		if err != nil {
			t.Fatal(err)
		}
		if err := ldg.Deposit(ctx, u.ID, 500_000_000, "seed-cash"); err != nil {
			t.Fatal(err)
		}
		x.totalCash += 500_000_000
		for _, tk := range tickers {
			if err := ldg.DepositShares(ctx, u.ID, tk.Symbol, tk.MakerShares, "seed:"+tk.Symbol); err != nil {
				t.Fatal(err)
			}
			x.totalShares[tk.Symbol] += tk.MakerShares
		}
	}
	return x
}

// running is a strategy trading as its maker account.
type running struct {
	client   *HTTPClient
	strategy Strategy
}

// startMakers logs every maker in with its production strategy.
func startMakers(t *testing.T, x *exchange) []running {
	c := NewHTTPClient(x.url, "mm-liquidity", "maker password")
	listed, err := c.Tickers(ctx)
	if err != nil || len(listed) != 10 {
		t.Fatalf("Tickers = %d, %v", len(listed), err)
	}
	return []running{
		{c, NewLiquidity(listed, 7)},
		{NewHTTPClient(x.url, "mm-flow", "maker password"), NewFlow(8)},
		{NewHTTPClient(x.url, "mm-momentum", "maker password"), NewMomentum(listed, 9)},
		{NewHTTPClient(x.url, "mm-news", "maker password"), NewNews(listed, 10)},
	}
}

// TestMakersMoveThePrice runs every strategy against a real in-process
// exchange over HTTP and checks that prices move with no other traders,
// stay near their references, and that the books balance.
func TestMakersMoveThePrice(t *testing.T) {
	x := newExchange(t)
	running := startMakers(t, x)
	log := slog.New(slog.DiscardHandler)

	moved := map[string]bool{}
	start := map[string]int64{}
	low, high := map[string]int64{}, map[string]int64{}
	var traded int
	for i := range 500 {
		snap, err := running[0].client.Prices(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			for _, p := range snap.Prices {
				start[p.Symbol], low[p.Symbol], high[p.Symbol] = p.Price, p.Price, p.Price
			}
		}
		for _, r := range running {
			trade(ctx, r.client, r.strategy, snap, log)
		}
		tr, err := x.eng.Tick(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range tr.Books {
			if b.Volume > 0 {
				traded++
				moved[b.Symbol] = moved[b.Symbol] || b.ClearingPrice != start[b.Symbol]
				low[b.Symbol] = min(low[b.Symbol], b.ClearingPrice)
				high[b.Symbol] = max(high[b.Symbol], b.ClearingPrice)
			}
		}
	}

	if len(moved) != 10 {
		t.Fatalf("only %d of 10 symbols moved: %v", len(moved), moved)
	}
	if traded < 1250 {
		t.Fatalf("only %d book clears in 500 ticks", traded)
	}
	var ranges float64
	for sym, s := range start {
		r := float64(high[sym]-low[sym]) / float64(s)
		ranges += r
		if float64(high[sym]) > float64(s)*1.25 || float64(low[sym]) < float64(s)*0.75 {
			t.Errorf("%s ranged %d to %d from %d in 500 ticks", sym, low[sym], high[sym], s)
		}
	}
	if avg := ranges / float64(len(start)); avg < 0.03 {
		t.Errorf("average range %.1f%% in 500 ticks; prices barely fluctuate", avg*100)
	}

	var cash int64
	shares := map[string]int64{}
	for _, name := range makers {
		c := NewHTTPClient(x.url, name, "maker password")
		a, err := c.Account(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if a.CashHeld != 0 {
			t.Errorf("%s has %d cash held after IOC-only trading", name, a.CashHeld)
		}
		cash += a.Cash
		for _, h := range a.Holdings {
			shares[h.Symbol] += h.Quantity
		}
	}
	if cash != x.totalCash {
		t.Errorf("cash %d, want %d", cash, x.totalCash)
	}
	for sym, n := range x.totalShares {
		if shares[sym] != n {
			t.Errorf("%s shares %d, want %d", sym, shares[sym], n)
		}
	}
}

// TestNewTradersFill checks the makers keep the books deep enough that a new
// player can move their whole starting $10,000 into any symbol and back out,
// using the web ticket's default prices, and every order fills in full within
// 1% of the last price.
func TestNewTradersFill(t *testing.T) {
	x := newExchange(t)
	running := startMakers(t, x)
	listed, err := running[0].client.Tickers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)

	// One trader per symbol, so every book is tested in the same auctions.
	traders := map[string]*HTTPClient{}
	for _, tk := range listed {
		name := "new-" + strings.ToLower(tk.Symbol)
		if _, err := x.gw.CreateUser(ctx, name, "trader password", gateway.RoleTrader); err != nil {
			t.Fatal(err)
		}
		traders[tk.Symbol] = NewHTTPClient(x.url, name, "trader password")
	}

	type pending struct {
		order  Order
		price  int64
		before Account
	}
	const warmup = 20
	for i := range warmup + 80 {
		snap, err := running[0].client.Prices(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range running {
			trade(ctx, r.client, r.strategy, snap, log)
		}

		// Each trader cycles all in by market order, all out by market
		// order, all in by limit, all out by limit.
		placed := map[string]pending{}
		for _, p := range snap.Prices {
			if i < warmup {
				break
			}
			c := traders[p.Symbol]
			a, err := c.Account(ctx)
			if err != nil {
				t.Fatal(err)
			}
			cash, held := a.Cash-a.CashHeld, holding(a, p.Symbol)
			var o Order
			switch (i - warmup) % 4 {
			case 0:
				// The ticket's default max cost is 5% over the last price.
				qty := int64(float64(cash) / (float64(p.Price) * 1.05))
				o = Order{Symbol: p.Symbol, Direction: "BUY", Type: "MARKET", Quantity: qty, MaxCost: int64(math.Ceil(float64(qty*p.Price) * 1.05))}
			case 1:
				o = Order{Symbol: p.Symbol, Direction: "SELL", Type: "MARKET", Quantity: held}
			case 2:
				limit := int64(math.Ceil(float64(p.Price) * 1.01))
				o = ioc(p.Symbol, "BUY", cash/limit, limit)
			case 3:
				o = ioc(p.Symbol, "SELL", held, int64(math.Floor(float64(p.Price)*0.99)))
			}
			if err := c.PlaceOrder(ctx, o); err != nil {
				t.Fatalf("tick %d: placing %+v: %v", i, o, err)
			}
			placed[p.Symbol] = pending{o, p.Price, a}
		}

		if _, err := x.eng.Tick(ctx); err != nil {
			t.Fatal(err)
		}

		for sym, pd := range placed {
			a, err := traders[sym].Account(ctx)
			if err != nil {
				t.Fatal(err)
			}
			o := pd.order
			if a.CashHeld != 0 {
				t.Errorf("tick %d: %s %s %s left %d cash held", i, o.Type, o.Direction, sym, a.CashHeld)
			}
			filled := holding(a, sym) - holding(pd.before, sym)
			paid := pd.before.Cash - a.Cash
			if o.Direction == "SELL" {
				filled, paid = -filled, -paid
			}
			if filled != o.Quantity {
				t.Errorf("tick %d: %s %s %d %s ($%d) filled %d", i, o.Type, o.Direction, o.Quantity, sym, o.Quantity*pd.price/100, filled)
				continue
			}
			slip := float64(paid)/float64(filled)/float64(pd.price) - 1
			if o.Direction == "SELL" {
				slip = -slip
			}
			if slip > 0.01 {
				t.Errorf("tick %d: %s %s %d %s filled %.2f%% worse than %d", i, o.Type, o.Direction, filled, sym, slip*100, pd.price)
			}
		}
	}
}

func holding(a Account, symbol string) int64 {
	for _, h := range a.Holdings {
		if h.Symbol == symbol {
			return h.Quantity
		}
	}
	return 0
}

func TestRunTradesOncePerTick(t *testing.T) {
	c := &fakeClient{snaps: []Snapshot{{Tick: 1}, {Tick: 1}, {Tick: 2}}}
	s := &countingStrategy{}
	ctx, cancel := context.WithCancel(ctx)
	c.onExhausted = cancel
	Run(ctx, c, s, Config{Poll: time.Millisecond, Delay: time.Millisecond, Logger: slog.New(slog.DiscardHandler)})
	if s.calls != 2 {
		t.Fatalf("strategy ran %d times for 2 distinct ticks", s.calls)
	}
}

type fakeClient struct {
	snaps       []Snapshot
	i           int
	onExhausted func()
}

func (f *fakeClient) Tickers(context.Context) ([]Ticker, error) { return nil, nil }
func (f *fakeClient) Prices(context.Context) (Snapshot, error) {
	if f.i >= len(f.snaps) {
		f.onExhausted()
		return f.snaps[len(f.snaps)-1], nil
	}
	f.i++
	return f.snaps[f.i-1], nil
}
func (f *fakeClient) Account(context.Context) (Account, error) { return Account{}, nil }
func (f *fakeClient) PlaceOrder(context.Context, Order) error  { return nil }

type countingStrategy struct{ calls int }

func (s *countingStrategy) Orders(Snapshot, Account) []Order { s.calls++; return nil }
