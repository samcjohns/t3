package reporting

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/samcjohns/t3/pkg/engine"
	"github.com/samcjohns/t3/pkg/ledger"
	"github.com/samcjohns/t3/pkg/listing"
)

var t0 = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func tick(n uint64, at time.Time, books ...engine.BookResult) engine.TickResult {
	return engine.TickResult{Tick: n, Timestamp: at, Books: books}
}

func book(symbol string, price int64, fills ...int64) engine.BookResult {
	b := engine.BookResult{Symbol: symbol, ClearingPrice: price}
	for i, q := range fills {
		b.Volume += q
		b.Executions = append(b.Executions, engine.Execution{
			Symbol: symbol, Price: price, Quantity: q,
			BuyOrderID: "b" + string(rune('0'+i)), BuyAccountID: "alice",
			SellOrderID: "s" + string(rune('0'+i)), SellAccountID: "bob",
		})
	}
	return b
}

func TestQuotesAndTape(t *testing.T) {
	s := New(nil, Config{})
	if _, err := s.Quote("ACME"); !errors.Is(err, ErrUnknownSymbol) {
		t.Fatalf("quote before trading: %v", err)
	}
	s.Ingest(tick(1, t0, book("ACME", 1000, 5, 3)))
	s.Ingest(tick(2, t0.Add(10*time.Second), book("ACME", 1010, 2), engine.BookResult{Symbol: "IDLE"}))

	q, err := s.Quote("ACME")
	if err != nil || q.LastPrice != 1010 || q.LastTick != 2 {
		t.Fatalf("quote = %+v, %v", q, err)
	}
	if _, err := s.Quote("IDLE"); !errors.Is(err, ErrUnknownSymbol) {
		t.Fatal("a book with no volume should not create a quote")
	}

	got := s.Trades("ACME", 2)
	want := []Trade{
		{Tick: 2, Time: t0.Add(10 * time.Second), Symbol: "ACME", Price: 1010, Quantity: 2},
		{Tick: 1, Time: t0, Symbol: "ACME", Price: 1000, Quantity: 3},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("trades = %+v", got)
	}
	if got := s.Trades("NONE", 0); got == nil || len(got) != 0 {
		t.Fatalf("unknown symbol trades = %#v, want empty", got)
	}
}

func TestIgnoresReplayedTicks(t *testing.T) {
	s := New(nil, Config{})
	s.Ingest(tick(1, t0, book("ACME", 1000, 5)))
	s.Ingest(tick(1, t0, book("ACME", 1000, 5)))
	if n := len(s.Trades("ACME", 0)); n != 1 {
		t.Fatalf("got %d trades after replay, want 1", n)
	}
}

func TestAccountTrades(t *testing.T) {
	s := New(nil, Config{})
	s.Ingest(tick(1, t0, book("ACME", 1000, 5)))

	alice := s.AccountTrades("alice", 0)
	if len(alice) != 1 || alice[0].Direction != engine.Buy || alice[0].OrderID != "b0" {
		t.Fatalf("alice = %+v", alice)
	}
	bob := s.AccountTrades("bob", 0)
	if len(bob) != 1 || bob[0].Direction != engine.Sell || bob[0].OrderID != "s0" {
		t.Fatalf("bob = %+v", bob)
	}
}

func TestHistoryIsCapped(t *testing.T) {
	s := New(nil, Config{MaxTradesPerSymbol: 3, MaxTradesPerAccount: 2})
	for i := range uint64(10) {
		s.Ingest(tick(i+1, t0, book("ACME", 1000+int64(i), 1)))
	}
	trades := s.Trades("ACME", 0)
	if len(trades) != 3 || trades[0].Price != 1009 || trades[2].Price != 1007 {
		t.Fatalf("tape = %+v", trades)
	}
	if n := len(s.AccountTrades("alice", 100)); n != 2 {
		t.Fatalf("account history = %d, want 2", n)
	}
}

func TestCandles(t *testing.T) {
	s := New(nil, Config{})
	prices := []struct {
		at    time.Duration
		price int64
		qty   int64
	}{
		{0, 1000, 1},
		{20 * time.Second, 1050, 2},
		{50 * time.Second, 990, 3},
		{70 * time.Second, 1010, 4}, // second minute
		{6 * time.Minute, 1100, 5},  // second 5-minute bucket
	}
	for i, p := range prices {
		s.Ingest(tick(uint64(i+1), t0.Add(p.at), book("ACME", p.price, p.qty)))
	}

	one, err := s.Candles("ACME", time.Minute, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []Candle{
		{Start: t0, Open: 1000, High: 1050, Low: 990, Close: 990, Volume: 6},
		{Start: t0.Add(time.Minute), Open: 1010, High: 1010, Low: 1010, Close: 1010, Volume: 4},
		{Start: t0.Add(6 * time.Minute), Open: 1100, High: 1100, Low: 1100, Close: 1100, Volume: 5},
	}
	if !reflect.DeepEqual(one, want) {
		t.Fatalf("1m candles = %+v", one)
	}

	five, _ := s.Candles("ACME", 5*time.Minute, 0)
	want5 := []Candle{
		{Start: t0, Open: 1000, High: 1050, Low: 990, Close: 1010, Volume: 10},
		{Start: t0.Add(5 * time.Minute), Open: 1100, High: 1100, Low: 1100, Close: 1100, Volume: 5},
	}
	if !reflect.DeepEqual(five, want5) {
		t.Fatalf("5m candles = %+v", five)
	}
	if last, _ := s.Candles("ACME", time.Minute, 1); len(last) != 1 || last[0].Close != 1100 {
		t.Fatalf("limited candles = %+v", last)
	}
	for _, bad := range []time.Duration{0, 30 * time.Second, 90 * time.Second} {
		if _, err := s.Candles("ACME", bad, 0); !errors.Is(err, ErrInvalidInterval) {
			t.Errorf("interval %v: %v", bad, err)
		}
	}
}

func TestPortfolio(t *testing.T) {
	l := ledger.New()
	must(t, l.OpenAccount(context.Background(), "alice"))
	must(t, l.Deposit(context.Background(), "alice", 50_000, ""))
	must(t, l.DepositShares(context.Background(), "alice", "ACME", 10, ""))
	must(t, l.DepositShares(context.Background(), "alice", "NEW", 4, ""))

	s := New(l, Config{})
	s.Ingest(tick(1, t0, book("ACME", 1200, 1)))

	p, err := s.Portfolio("alice")
	must(t, err)
	want := Portfolio{
		AccountID: "alice", Cash: 50_000,
		Positions: []Position{
			{Symbol: "ACME", Quantity: 10, LastPrice: 1200, Value: 12_000},
			{Symbol: "NEW", Quantity: 4}, // never traded: unvalued
		},
		MarketValue: 12_000, TotalValue: 62_000,
	}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("portfolio = %+v", p)
	}
	if _, err := s.Portfolio("nobody"); !errors.Is(err, ledger.ErrUnknownAccount) {
		t.Fatalf("unknown account: %v", err)
	}
}

// trade is one execution between alice and bob, with alice buying or selling.
func trade(symbol string, price, qty int64, aliceBuys bool, id string) engine.BookResult {
	e := engine.Execution{Symbol: symbol, Price: price, Quantity: qty,
		BuyOrderID: "b" + id, BuyAccountID: "alice", SellOrderID: "s" + id, SellAccountID: "bob"}
	if !aliceBuys {
		e.BuyAccountID, e.SellAccountID = "bob", "alice"
	}
	return engine.BookResult{Symbol: symbol, ClearingPrice: price, Volume: qty, Executions: []engine.Execution{e}}
}

// settle runs ticks through both the ledger and the reporting service.
func settle(t *testing.T, l *ledger.Ledger, s *Service, trs ...engine.TickResult) {
	t.Helper()
	for _, tr := range trs {
		must(t, l.ApplyTick(context.Background(), tr))
		s.Ingest(tr)
	}
}

func order(id, account string, dir engine.Direction, qty, limit int64) engine.Order {
	return engine.Order{ID: id, AccountID: account, Symbol: "ACME", Direction: dir, Type: engine.Limit, Quantity: qty, LimitPrice: limit}
}

func TestCostBasisAndHistory(t *testing.T) {
	ctx := context.Background()
	l := ledger.New()
	for _, a := range []string{"alice", "bob"} {
		must(t, l.OpenAccount(ctx, a))
		must(t, l.Deposit(ctx, a, 100_000, ""))
	}
	must(t, l.DepositShares(ctx, "bob", "ACME", 100, ""))
	s := New(l, Config{})

	// Alice buys 10 @ 1000 and 10 @ 1200, then sells 5 @ 1100.
	must(t, l.Reserve(ctx, order("b1", "alice", engine.Buy, 10, 1000)))
	must(t, l.Reserve(ctx, order("s1", "bob", engine.Sell, 10, 1000)))
	settle(t, l, s, tick(1, t0.Add(10*time.Second), trade("ACME", 1000, 10, true, "1")))
	must(t, l.Reserve(ctx, order("b2", "alice", engine.Buy, 10, 1200)))
	must(t, l.Reserve(ctx, order("s2", "bob", engine.Sell, 10, 1200)))
	settle(t, l, s, tick(2, t0.Add(2*time.Minute), trade("ACME", 1200, 10, true, "2")))
	must(t, l.Reserve(ctx, order("b3", "bob", engine.Buy, 5, 1100)))
	must(t, l.Reserve(ctx, order("s3", "alice", engine.Sell, 5, 1100)))
	settle(t, l, s, tick(3, t0.Add(4*time.Minute+30*time.Second), trade("ACME", 1100, 5, false, "3")))
	// Someone else trades ACME at 1300 in a later minute.
	s.Ingest(tick(4, t0.Add(6*time.Minute), book("ACME", 1300)))
	s.Ingest(tick(5, t0.Add(6*time.Minute+10*time.Second), engine.BookResult{Symbol: "ACME", ClearingPrice: 1300, Volume: 1,
		Executions: []engine.Execution{{Symbol: "ACME", Price: 1300, Quantity: 1, BuyAccountID: "x", SellAccountID: "y"}}}))

	p, err := s.Portfolio("alice")
	must(t, err)
	// Average cost 1100 a share; selling keeps it, leaving 15 * 1100.
	if pos := p.Positions[0]; pos.Quantity != 15 || pos.CostBasis == nil || *pos.CostBasis != 16_500 {
		t.Fatalf("alice position = %+v", pos)
	}
	// Bob's deposited shares have no known cost.
	if p, _ := s.Portfolio("bob"); p.Positions[0].CostBasis != nil {
		t.Fatalf("bob position = %+v", p.Positions[0])
	}

	// Alice's cash: 100,000 - 10,000 - 12,000 + 5,500 = 83,500.
	one, err := s.AccountHistory("alice", time.Minute, 0)
	must(t, err)
	want := []ValueCandle{
		{Start: t0, Open: 100_000, High: 100_000, Low: 100_000, Close: 100_000},
		{Start: t0.Add(time.Minute), Open: 100_000, High: 100_000, Low: 100_000, Close: 100_000},
		{Start: t0.Add(2 * time.Minute), Open: 100_000, High: 102_000, Low: 100_000, Close: 102_000},
		{Start: t0.Add(3 * time.Minute), Open: 102_000, High: 102_000, Low: 102_000, Close: 102_000},
		{Start: t0.Add(4 * time.Minute), Open: 102_000, High: 102_000, Low: 100_000, Close: 100_000},
		{Start: t0.Add(5 * time.Minute), Open: 100_000, High: 100_000, Low: 100_000, Close: 100_000},
		{Start: t0.Add(6 * time.Minute), Open: 100_000, High: 103_000, Low: 100_000, Close: 103_000},
	}
	if !reflect.DeepEqual(one, want) {
		t.Fatalf("1m history =\n%+v\nwant\n%+v", one, want)
	}
	if last := one[len(one)-1].Close; last != p.TotalValue {
		t.Fatalf("history ends at %d, portfolio is worth %d", last, p.TotalValue)
	}

	five, _ := s.AccountHistory("alice", 5*time.Minute, 0)
	want5 := []ValueCandle{
		{Start: t0, Open: 100_000, High: 102_000, Low: 100_000, Close: 100_000},
		{Start: t0.Add(5 * time.Minute), Open: 100_000, High: 103_000, Low: 100_000, Close: 103_000},
	}
	if !reflect.DeepEqual(five, want5) {
		t.Fatalf("5m history = %+v", five)
	}

	// A limit keeps the newest periods, rewound to the window's start.
	lim, _ := s.AccountHistory("alice", time.Minute, 3)
	if len(lim) != 3 || lim[0].Start != t0.Add(4*time.Minute) || lim[0].Open != 102_000 || lim[2].Close != 103_000 {
		t.Fatalf("limited history = %+v", lim)
	}
	if _, err := s.AccountHistory("alice", 90*time.Second, 0); !errors.Is(err, ErrInvalidInterval) {
		t.Fatalf("bad interval: %v", err)
	}
	if h, _ := s.AccountHistory("nobody", time.Minute, 0); h != nil {
		t.Fatalf("unknown account history = %+v", h)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type tickSource []engine.TickResult

func (s tickSource) TicksAfter(_ context.Context, after uint64, limit int) ([]engine.TickResult, error) {
	var out []engine.TickResult
	for _, tr := range s {
		if tr.Tick > after && len(out) < limit {
			out = append(out, tr)
		}
	}
	return out, nil
}

func TestReplay(t *testing.T) {
	var src tickSource
	for i := range uint64(2500) {
		src = append(src, tick(i+1, t0.Add(time.Duration(i)*10*time.Second), book("ACME", 1000+int64(i%7), 1)))
	}
	s := New(nil, Config{})
	must(t, s.Replay(context.Background(), src))
	q, _ := s.Quote("ACME")
	if q.LastTick != 2500 || len(s.Trades("ACME", 0)) != 1000 {
		t.Fatalf("after replay: quote %+v, %d trades", q, len(s.Trades("ACME", 0)))
	}
}

func TestPriceSnapshot(t *testing.T) {
	tickers := []listing.Ticker{{Symbol: "ACME", Name: "Acme", ReferencePrice: 1000}, {Symbol: "ZED", Name: "Zed", ReferencePrice: 500}}
	s := New(nil, Config{Listing: tickers, Heartbeat: 10 * time.Second})
	decode := func() PriceSnapshot {
		var p PriceSnapshot
		if err := json.Unmarshal(s.Prices().JSON, &p); err != nil {
			t.Fatal(err)
		}
		return p
	}

	p := decode()
	if p.Tick != 0 || p.Prices[0].Price != 1000 || p.Prices[1].PreviousClose != 500 || s.Prices().ETag != `"t0"` {
		t.Fatalf("initial snapshot = %+v", p)
	}

	s.Ingest(tick(1, t0, book("ACME", 1100, 3)))
	s.Ingest(tick(2, t0.Add(10*time.Second), book("ACME", 1150, 2)))
	p = decode()
	want := Price{Symbol: "ACME", Name: "Acme", Price: 1150, PreviousClose: 1000, Change: 150, Volume: 5, LastTradeTick: 2}
	if p.Tick != 2 || p.Prices[0] != want || !p.NextTickAt.Equal(t0.Add(20*time.Second)) || s.Prices().ETag != `"t2"` {
		t.Fatalf("snapshot = %+v", p)
	}
	if p.Prices[1].Price != 500 || p.Prices[1].LastTradeTick != 0 {
		t.Fatalf("untraded symbol = %+v", p.Prices[1])
	}

	// A new UTC day rolls the close forward and resets volume.
	s.Ingest(tick(3, t0.Add(24*time.Hour), engine.BookResult{Symbol: "ACME"}))
	if got := decode().Prices[0]; got.PreviousClose != 1150 || got.Change != 0 || got.Volume != 0 {
		t.Fatalf("after day roll = %+v", got)
	}
}

func BenchmarkPrices(b *testing.B) {
	s := New(nil, Config{Listing: listing.Default()})
	s.Ingest(tick(1, t0, book("ACME", 1100, 3)))
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = s.Prices()
		}
	})
}
