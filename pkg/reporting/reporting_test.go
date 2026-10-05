package reporting

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/samcjohns/t3/pkg/engine"
	"github.com/samcjohns/t3/pkg/ledger"
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
	must(t, l.OpenAccount("alice"))
	must(t, l.Deposit("alice", 50_000))
	must(t, l.DepositShares("alice", "ACME", 10))
	must(t, l.DepositShares("alice", "NEW", 4))

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

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
