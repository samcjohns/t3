package ledger

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"testing"

	"github.com/samcjohns/t3/pkg/engine"
)

func buy(id, acct string, qty, price int64) engine.Order {
	return engine.Order{ID: id, AccountID: acct, Symbol: "ACME", Direction: engine.Buy, Type: engine.Limit, Quantity: qty, LimitPrice: price}
}

func sell(id, acct string, qty, price int64) engine.Order {
	return engine.Order{ID: id, AccountID: acct, Symbol: "ACME", Direction: engine.Sell, Type: engine.Limit, Quantity: qty, LimitPrice: price}
}

func marketBuy(id, acct string, qty, maxCost int64) engine.Order {
	return engine.Order{ID: id, AccountID: acct, Symbol: "ACME", Direction: engine.Buy, Type: engine.Market, Quantity: qty, MaxCost: maxCost}
}

// market wires a ledger to a real orchestrator, the way the services will
// run: reserve, then submit, with ticks settled into the ledger.
type market struct {
	t   *testing.T
	l   *Ledger
	eng *engine.Orchestrator
}

func newMarket(t *testing.T) *market {
	m := &market{t: t, l: New()}
	m.eng = engine.NewOrchestrator(engine.FBAMatcher{}, engine.Config{OnTick: func(tr engine.TickResult) {
		if err := m.l.ApplyTick(tr); err != nil {
			t.Errorf("ApplyTick: %v", err)
		}
	}})
	return m
}

func (m *market) fund(acct string, cash, shares int64) {
	m.t.Helper()
	must(m.t, m.l.OpenAccount(acct))
	if cash > 0 {
		must(m.t, m.l.Deposit(acct, cash))
	}
	if shares > 0 {
		must(m.t, m.l.DepositShares(acct, "ACME", shares))
	}
}

func (m *market) place(o engine.Order) error {
	if err := m.l.Reserve(o); err != nil {
		return err
	}
	if _, err := m.eng.Submit(o); err != nil {
		must(m.t, m.l.Release(o.ID))
		return err
	}
	return nil
}

func (m *market) want(acct string, cash, cashHeld, shares, sharesHeld int64) {
	m.t.Helper()
	a, err := m.l.Account(acct)
	must(m.t, err)
	want := Account{ID: acct, Cash: cash, CashHeld: cashHeld, Holdings: []Holding{}}
	if shares > 0 {
		want.Holdings = []Holding{{Symbol: "ACME", Quantity: shares, Held: sharesHeld}}
	}
	if !reflect.DeepEqual(a, want) {
		m.t.Fatalf("account %s = %+v, want %+v", acct, a, want)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestAccountSetup(t *testing.T) {
	l := New()
	must(t, l.OpenAccount("a"))
	if err := l.OpenAccount("a"); !errors.Is(err, ErrAccountExists) {
		t.Errorf("reopen: %v", err)
	}
	if err := l.Deposit("nobody", 1); !errors.Is(err, ErrUnknownAccount) {
		t.Errorf("deposit to unknown: %v", err)
	}
	for _, amt := range []int64{0, -5} {
		if err := l.Deposit("a", amt); !errors.Is(err, ErrInvalidAmount) {
			t.Errorf("deposit %d: %v", amt, err)
		}
	}
	must(t, l.Deposit("a", math.MaxInt64))
	if err := l.Deposit("a", 1); !errors.Is(err, ErrInvalidAmount) {
		t.Errorf("overflowing deposit: %v", err)
	}
	if err := l.DepositShares("a", "", 1); !errors.Is(err, ErrInvalidAmount) {
		t.Errorf("deposit without symbol: %v", err)
	}
}

func TestReserveRejectsUnfundedOrders(t *testing.T) {
	l := New()
	must(t, l.OpenAccount("a"))
	must(t, l.Deposit("a", 10_000))
	must(t, l.DepositShares("a", "ACME", 5))

	cases := []struct {
		order engine.Order
		err   error
	}{
		{buy("1", "a", 11, 1000), ErrInsufficientFunds},
		{marketBuy("2", "a", 1, 10_001), ErrInsufficientFunds},
		{sell("3", "a", 6, 1), ErrInsufficientShares},
		{buy("4", "nobody", 1, 1), ErrUnknownAccount},
		{buy("5", "a", 2, math.MaxInt64), ErrInvalidAmount},
		{marketBuy("6", "a", 1, 0), engine.ErrInvalidOrder},
	}
	for _, c := range cases {
		if err := l.Reserve(c.order); !errors.Is(err, c.err) {
			t.Errorf("Reserve(%s) = %v, want %v", c.order.ID, err, c.err)
		}
	}

	// Holds reduce what later orders can use.
	must(t, l.Reserve(buy("7", "a", 6, 1000)))
	if err := l.Reserve(buy("8", "a", 5, 1000)); !errors.Is(err, ErrInsufficientFunds) {
		t.Errorf("second buy over available: %v", err)
	}
	if err := l.Reserve(buy("7", "a", 1, 1)); !errors.Is(err, ErrDuplicateHold) {
		t.Errorf("duplicate hold: %v", err)
	}
	must(t, l.Reserve(sell("9", "a", 5, 1)))
	if err := l.Reserve(sell("10", "a", 1, 1)); !errors.Is(err, ErrInsufficientShares) {
		t.Errorf("sell over available shares: %v", err)
	}

	must(t, l.Release("7"))
	must(t, l.Release("9"))
	if err := l.Release("7"); !errors.Is(err, ErrUnknownHold) {
		t.Errorf("double release: %v", err)
	}
	a, _ := l.Account("a")
	if a.CashHeld != 0 || a.Holdings[0].Held != 0 {
		t.Fatalf("after releases: %+v", a)
	}
}

func TestSettlementWithPriceImprovement(t *testing.T) {
	m := newMarket(t)
	m.fund("alice", 100_000, 0)
	m.fund("bob", 0, 30)

	must(t, m.place(buy("b1", "alice", 20, 1010)))
	must(t, m.place(sell("s1", "bob", 10, 1000)))
	m.want("alice", 100_000, 20_200, 0, 0)
	m.want("bob", 0, 0, 30, 10)

	// Clears 10 @ 1005. Alice pays 10_050 and gets 50 of her 10_100 hold
	// for those shares back; 10 shares stay resting at 1010.
	m.eng.Tick()
	m.want("alice", 89_950, 10_100, 10, 0)
	m.want("bob", 10_050, 0, 20, 0)

	must(t, m.place(sell("s2", "bob", 20, 900)))
	// Clears 10 @ 955, the midpoint of 900 and 1010. Alice's buy completes,
	// releasing the rest of its hold; bob keeps 10 shares resting.
	m.eng.Tick()
	m.want("alice", 80_400, 0, 20, 0)
	m.want("bob", 19_600, 0, 10, 10)
}

func TestMarketBuyHoldReleasedOnExpiry(t *testing.T) {
	m := newMarket(t)
	m.fund("alice", 50_000, 0)
	m.fund("bob", 0, 100)

	must(t, m.place(sell("s1", "bob", 100, 1000)))
	must(t, m.place(marketBuy("m1", "alice", 100, 25_500)))
	m.want("alice", 50_000, 25_500, 0, 0)

	// max_cost covers 25 shares at 1000; the rest expires and the 500
	// left in the hold is released.
	m.eng.Tick()
	m.want("alice", 25_000, 0, 25, 0)
	m.want("bob", 25_000, 0, 75, 75)
}

func TestMarketBuyHoldReleasedOnFullFill(t *testing.T) {
	m := newMarket(t)
	m.fund("alice", 50_000, 0)
	m.fund("bob", 0, 100)

	must(t, m.place(sell("s1", "bob", 100, 1000)))
	must(t, m.place(marketBuy("m1", "alice", 10, 50_000)))
	m.eng.Tick()
	m.want("alice", 40_000, 0, 10, 0)
}

func TestTickOrdering(t *testing.T) {
	l := New()
	if err := l.ApplyTick(engine.TickResult{Tick: 2}); !errors.Is(err, ErrTickOutOfOrder) {
		t.Errorf("gap: %v", err)
	}
	must(t, l.ApplyTick(engine.TickResult{Tick: 1}))
	if err := l.ApplyTick(engine.TickResult{Tick: 1}); !errors.Is(err, ErrTickAlreadyApplied) {
		t.Errorf("replay: %v", err)
	}
	if l.LastTick() != 1 {
		t.Errorf("LastTick = %d", l.LastTick())
	}
}

func TestInconsistentTickChangesNothing(t *testing.T) {
	l := New()
	for _, acct := range []string{"alice", "bob"} {
		must(t, l.OpenAccount(acct))
	}
	must(t, l.Deposit("alice", 100_000))
	must(t, l.DepositShares("bob", "ACME", 10))
	must(t, l.Reserve(buy("b1", "alice", 10, 1000)))
	must(t, l.Reserve(sell("s1", "bob", 10, 1000)))
	must(t, l.Reserve(marketBuy("m1", "alice", 10, 5_000)))

	good := engine.Execution{Symbol: "ACME", BuyOrderID: "b1", SellOrderID: "s1", BuyAccountID: "alice", SellAccountID: "bob", Price: 1000, Quantity: 5}
	tweak := func(f func(*engine.Execution)) engine.Execution { e := good; f(&e); return e }

	cases := map[string]engine.BookResult{
		"unknown order":   {Executions: []engine.Execution{good, tweak(func(e *engine.Execution) { e.BuyOrderID = "ghost" })}},
		"wrong account":   {Executions: []engine.Execution{good, tweak(func(e *engine.Execution) { e.SellAccountID = "alice" })}},
		"wrong symbol":    {Executions: []engine.Execution{tweak(func(e *engine.Execution) { e.Symbol = "ZED" })}},
		"through limit":   {Executions: []engine.Execution{tweak(func(e *engine.Execution) { e.Price = 1001 })}},
		"overfill":        {Executions: []engine.Execution{good, good, good}},
		"over max_cost":   {Executions: []engine.Execution{tweak(func(e *engine.Execution) { e.BuyOrderID = "m1"; e.Quantity = 6 })}},
		"cost overflow":   {Executions: []engine.Execution{tweak(func(e *engine.Execution) { e.BuyOrderID = "m1"; e.Price = math.MaxInt64 })}},
		"zero quantity":   {Executions: []engine.Execution{tweak(func(e *engine.Execution) { e.Quantity = 0 })}},
		"expire unknown":  {Executions: []engine.Execution{good}, ExpiredOrderIDs: []string{"ghost"}},
		"expire twice":    {ExpiredOrderIDs: []string{"m1", "m1"}},
		"fill after exp.": {ExpiredOrderIDs: []string{"b1"}, Executions: nil},
	}
	before := func() []Account {
		a, _ := l.Account("alice")
		b, _ := l.Account("bob")
		return []Account{a, b}
	}
	snapshot := before()
	for name, book := range cases {
		t.Run(name, func(t *testing.T) {
			book.Symbol = "ACME"
			tr := engine.TickResult{Tick: 1, Books: []engine.BookResult{book}}
			if name == "fill after exp." {
				tr.Books = append(tr.Books, engine.BookResult{Symbol: "ACME", Executions: []engine.Execution{good}})
			}
			if err := l.ApplyTick(tr); !errors.Is(err, ErrInconsistentTick) {
				t.Fatalf("err = %v, want ErrInconsistentTick", err)
			}
			if !reflect.DeepEqual(before(), snapshot) || l.LastTick() != 0 {
				t.Fatal("rejected tick changed state")
			}
		})
	}
}

// TestConservation runs many random ticks and checks that cash and shares
// are never created or destroyed and holds never exceed balances.
func TestConservation(t *testing.T) {
	m := newMarket(t)
	rng := rand.New(rand.NewPCG(3, 4))
	accts := []string{"a", "b", "c", "d", "e"}
	const cash, shares = 1_000_000, 500
	for _, a := range accts {
		m.fund(a, cash, shares)
	}

	id := 0
	for range 300 {
		for range 1 + rng.IntN(8) {
			id++
			acct := accts[rng.IntN(len(accts))]
			qty := 1 + rng.Int64N(50)
			price := 900 + rng.Int64N(200)
			var o engine.Order
			switch rng.IntN(4) {
			case 0:
				o = buy(fmt.Sprint(id), acct, qty, price)
			case 1:
				o = marketBuy(fmt.Sprint(id), acct, qty, qty*price)
			default:
				o = sell(fmt.Sprint(id), acct, qty, price)
			}
			if err := m.place(o); err != nil && !errors.Is(err, ErrInsufficientFunds) && !errors.Is(err, ErrInsufficientShares) {
				t.Fatal(err)
			}
		}
		m.eng.Tick()

		var totalCash, totalShares int64
		for _, a := range accts {
			acc, err := m.l.Account(a)
			must(t, err)
			if acc.CashHeld < 0 || acc.CashHeld > acc.Cash {
				t.Fatalf("bad cash hold: %+v", acc)
			}
			totalCash += acc.Cash
			for _, h := range acc.Holdings {
				if h.Held < 0 || h.Held > h.Quantity {
					t.Fatalf("bad share hold: %+v", acc)
				}
				totalShares += h.Quantity
			}
		}
		if totalCash != cash*int64(len(accts)) || totalShares != shares*int64(len(accts)) {
			t.Fatalf("conservation broken: cash %d shares %d", totalCash, totalShares)
		}
	}
	if m.l.LastTick() != 300 {
		t.Fatalf("LastTick = %d", m.l.LastTick())
	}
}
