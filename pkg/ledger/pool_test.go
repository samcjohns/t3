package ledger

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/samcjohns/t3/pkg/engine"
)

// tenTicksADay makes interest large enough to see: 2x loans pay 0.5% a tick.
// Like the other tests' configs, it lifts the loan limits, which
// TestLoanLimits covers, so small pools can fund large loans.
var tenTicksADay = Config{Leverage: unlimited(Leverage{TicksPerDay: 10})}

// unlimited lets one trade, or one trader, borrow the whole pool.
func unlimited(l Leverage) Leverage {
	l.MaxTradeLoan, l.MaxTraderLoan = 10_000, 10_000
	return l
}

func ioc(o engine.Order) engine.Order {
	o.TimeInForce = engine.IOC
	return o
}

func (m *market) placeLeveraged(o engine.Order, ratio int64) error {
	if err := m.l.ReserveLeveraged(ctx, o, ratio); err != nil {
		return err
	}
	if _, err := m.eng.Submit(ctx, o); err != nil {
		must(m.t, m.l.Release(ctx, o.ID))
		return err
	}
	return nil
}

// liquidate carries out the ledger's liquidation steps, as the server does
// after every tick.
func (m *market) liquidate() Liquidation {
	m.t.Helper()
	liq, err := m.l.Liquidations(ctx)
	must(m.t, err)
	for _, id := range liq.Cancel {
		m.eng.Cancel(id)
	}
	for _, o := range liq.Sell {
		if _, err := m.eng.Submit(ctx, o); err != nil {
			m.t.Fatal(err)
		}
	}
	return liq
}

func (m *market) account(id string) Account {
	m.t.Helper()
	a, err := m.l.Account(id)
	must(m.t, err)
	return a
}

// conserved checks that cash only moves between accounts and the pool.
func (m *market) conserved(total int64) {
	m.t.Helper()
	sum := m.l.pool.cash
	for id := range m.l.accounts {
		sum += m.account(id).Cash
	}
	if sum != total {
		m.t.Fatalf("accounts and pool hold %d cash, want %d", sum, total)
	}
	if p := m.l.pool; p.reserved < 0 || p.reserved > p.cash {
		m.t.Fatalf("pool reserves %d of %d", p.reserved, p.cash)
	}
}

func TestLendAndRedeem(t *testing.T) {
	m := newMarket(t)
	m.fund("lena", 10_000, 0)
	m.fund("leo", 5_000, 0)
	must(t, m.l.Lend(ctx, "lena", 10_000))
	must(t, m.l.Lend(ctx, "leo", 4_000))
	if a := m.account("lena"); a.Cash != 0 || a.PoolShares != 10_000*initialSharesPerCent || a.PoolValue != 10_000 {
		t.Fatalf("lena = %+v", a)
	}
	if p := m.l.Pool(); p.Value != 14_000 || p.Available != 14_000 || p.Lenders != 2 || p.LenderRate != 0 {
		t.Fatalf("pool = %+v", p)
	}
	if err := m.l.Lend(ctx, "leo", 1_001); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("lending more than cash: %v", err)
	}

	if got, err := m.l.Redeem(ctx, "lena", 2_500); err != nil || got != 2_500 {
		t.Fatalf("Redeem = %d, %v", got, err)
	}
	if a := m.account("lena"); a.Cash != 2_500 || a.PoolValue != 7_500 {
		t.Fatalf("lena = %+v", a)
	}
	if _, err := m.l.Redeem(ctx, "lena", 7_501); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("redeeming more than the stake: %v", err)
	}
	if got, err := m.l.Redeem(ctx, "lena", 0); err != nil || got != 7_500 {
		t.Fatalf("Redeem all = %d, %v", got, err)
	}
	if a := m.account("lena"); a.Cash != 10_000 || a.PoolShares != 0 {
		t.Fatalf("lena = %+v", a)
	}
	if _, err := m.l.Redeem(ctx, "lena", 0); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("redeeming an empty stake: %v", err)
	}
	m.conserved(15_000)
}

func TestLeveragedBuyBorrowsAccruesAndRepays(t *testing.T) {
	m := newMarketWith(t, tenTicksADay)
	m.fund("lena", 100_000, 0)
	m.fund("tom", 10_000, 0)
	m.fund("sam", 0, 100)
	m.fund("bea", 50_000, 0)
	must(t, m.l.Lend(ctx, "lena", 100_000))

	must(t, m.placeLeveraged(ioc(buy("b1", "tom", 20, 1000)), 2))
	if a, p := m.account("tom"), m.l.Pool(); a.CashHeld != 10_000 || p.Available != 90_000 {
		t.Fatalf("after reserve: tom %+v, pool %+v", a, p)
	}
	must(t, m.place(sell("s1", "sam", 20, 1000)))
	m.tick()

	// Borrowed 10,000, plus one tick of interest at 0.5%.
	tom := m.account("tom")
	if tom.Cash != 0 || tom.CashHeld != 0 || tom.Owed != 10_050 || tom.Holdings[0].Quantity != 20 {
		t.Fatalf("tom = %+v", tom)
	}
	p := m.l.Pool()
	if p.Value != 100_050 || p.Lent != 10_050 || p.Available != 90_000 || p.Borrowers != 1 || p.LenderRate != 50 {
		t.Fatalf("pool = %+v", p)
	}
	if lena := m.account("lena"); lena.PoolValue != 100_050 {
		t.Fatalf("lena's stake = %d", lena.PoolValue)
	}
	if err := m.l.Lend(ctx, "tom", 1); !errors.Is(err, ErrLeverage) {
		t.Fatalf("a borrower lending: %v", err)
	}

	m.tick() // 10,000 × 1.005² = 10,100.25
	if tom := m.account("tom"); tom.Owed != 10_101 {
		t.Fatalf("owed after two ticks = %d", tom.Owed)
	}

	// Selling repays the loan from the proceeds.
	must(t, m.place(sell("s2", "tom", 20, 1100)))
	must(t, m.place(buy("b2", "bea", 20, 1100)))
	m.tick()
	tom = m.account("tom")
	if tom.Cash != 22_000-10_101 || tom.Owed != 0 || tom.Loans != nil {
		t.Fatalf("tom after selling = %+v", tom)
	}
	if p := m.l.Pool(); p.Value != 100_101 || p.Lent != 0 || p.Borrowers != 0 {
		t.Fatalf("pool = %+v", p)
	}
	if lena := m.account("lena"); lena.PoolValue != 100_101 {
		t.Fatalf("lena's stake = %d, want her money back with interest", lena.PoolValue)
	}
	m.conserved(160_000)
}

func TestLeveragedBuyWithPriceImprovement(t *testing.T) {
	m := newMarketWith(t, Config{Leverage: unlimited(Leverage{TicksPerDay: 1 << 40})})
	m.fund("lena", 100_000, 0)
	m.fund("tom", 10_000, 0)
	m.fund("sam", 0, 100)
	must(t, m.l.Lend(ctx, "lena", 100_000))

	// 3x at a 1,500 limit: tom holds 5,000 and the pool sets aside 10,000.
	must(t, m.placeLeveraged(buy("b1", "tom", 10, 1500), 3))
	must(t, m.place(sell("s1", "sam", 4, 1200)))
	m.tick()
	// 4 fill at the 1,350 midpoint = 5,400: 3,600 borrowed and 1,800 paid
	// by tom, plus a cent of interest. The hold releases its share at the
	// limit: 2,000 cash and 4,000 of loan.
	tom := m.account("tom")
	if tom.Cash != 10_000-1_800 || tom.CashHeld != 3_000 || tom.Owed != 3_601 {
		t.Fatalf("tom = %+v", tom)
	}
	if p := m.l.pool; p.reserved != 6_000 {
		t.Fatalf("pool reserves %d", p.reserved)
	}
	m.conserved(110_000)
}

func TestLeverageRules(t *testing.T) {
	m := newMarketWith(t, Config{Leverage: unlimited(Leverage{})})
	m.fund("lena", 10_000, 0)
	m.fund("tom", 10_000, 10)
	must(t, m.l.Lend(ctx, "lena", 5_000))

	for _, c := range []struct {
		name  string
		o     engine.Order
		ratio int64
		want  error
	}{
		{"sell", sell("s", "tom", 1, 100), 2, ErrLeverage},
		{"unlisted ratio", buy("b", "tom", 1, 100), 4, ErrLeverage},
		{"lender", buy("b", "lena", 1, 100), 2, ErrLeverage},
		{"pool too small", buy("b", "tom", 101, 100), 2, ErrPoolLiquidity},
		{"own share unfunded", buy("b", "tom", 1_000, 100), 5, ErrInsufficientFunds},
		{"fits", buy("b", "tom", 100, 100), 2, nil},
	} {
		err := m.l.ReserveLeveraged(ctx, c.o, c.ratio)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
		if err == nil {
			must(t, m.l.Release(ctx, c.o.ID))
		}
	}
	if m.l.pool.reserved != 0 {
		t.Fatalf("released holds left %d reserved", m.l.pool.reserved)
	}
}

func TestLiquidationAndBankruptcy(t *testing.T) {
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	m := newMarketWith(t, Config{Clock: func() time.Time { return now }, Leverage: unlimited(Leverage{TicksPerDay: 10})})
	m.fund("lena", 100_000, 0)
	m.fund("tom", 10_000, 0)
	m.fund("sam", 0, 100)
	m.fund("bea", 100_000, 0)
	must(t, m.l.Lend(ctx, "lena", 100_000))

	// 5x: 50 shares for 10,000 of tom's cash and 40,000 borrowed.
	must(t, m.placeLeveraged(ioc(buy("b1", "tom", 50, 1000)), 5))
	must(t, m.place(sell("s1", "sam", 50, 1000)))
	m.tick()
	if tom := m.account("tom"); tom.Status != StatusActive || tom.Owed != 40_280 {
		t.Fatalf("tom = %+v", tom)
	}
	// A resting order that liquidation must cancel.
	must(t, m.place(sell("gtc", "tom", 10, 5000)))
	if liq := m.liquidate(); liq.Cancel != nil || liq.Sell != nil {
		t.Fatalf("nothing to liquidate yet, got %+v", liq)
	}

	// ACME trades at 800, and tom's equity goes negative.
	must(t, m.place(sell("s2", "sam", 1, 800)))
	must(t, m.place(buy("b2", "bea", 1, 800)))
	m.tick()
	if tom := m.account("tom"); tom.Status != StatusLiquidating {
		t.Fatalf("tom = %+v, want liquidating", tom)
	}
	if err := m.place(sell("s3", "tom", 1, 1)); !errors.Is(err, ErrAccountLiquidating) {
		t.Fatalf("trading while liquidating: %v", err)
	}

	liq := m.liquidate()
	if !reflect.DeepEqual(liq.Cancel, []string{"gtc"}) || len(liq.Sell) != 1 || liq.Sell[0].Quantity != 40 {
		t.Fatalf("liquidation = %+v", liq)
	}
	must(t, m.place(buy("b3", "bea", 50, 700)))
	m.tick()
	if tom := m.account("tom"); tom.Status != StatusLiquidating || tom.Holdings[0].Quantity != 10 || tom.Holdings[0].Held != 0 {
		t.Fatalf("tom after the first sale = %+v", tom)
	}
	if liq := m.liquidate(); liq.Cancel != nil || len(liq.Sell) != 1 || liq.Sell[0].Quantity != 10 {
		t.Fatalf("second liquidation = %+v", liq)
	}
	m.tick()

	// 35,000 of sales repaid most of the loan. The rest is written off and
	// lena's stake bears the loss.
	tom := m.account("tom")
	if tom.Status != StatusBankrupt || tom.Owed != 0 || tom.Cash != 0 || len(tom.Holdings) != 0 {
		t.Fatalf("tom = %+v", tom)
	}
	b := m.l.Bankruptcies()
	if len(b) != 1 || b[0].AccountID != "tom" || b[0].Tick != 4 || !b[0].Time.Equal(now) || b[0].WrittenOff < 5_000 || b[0].WrittenOff > 6_000 {
		t.Fatalf("bankruptcies = %+v", b)
	}
	if p := m.l.Pool(); p.Value != 95_000 || p.Lent != 0 || p.Borrowers != 0 {
		t.Fatalf("pool = %+v", p)
	}
	if lena := m.account("lena"); lena.PoolValue != 95_000 {
		t.Fatalf("lena's stake = %d", lena.PoolValue)
	}
	if err := m.place(buy("b4", "tom", 1, 1)); !errors.Is(err, ErrAccountClosed) {
		t.Fatalf("trading when bankrupt: %v", err)
	}
	if liq := m.liquidate(); liq.Cancel != nil || liq.Sell != nil {
		t.Fatalf("liquidation after bankruptcy = %+v", liq)
	}
	m.conserved(210_000)
}

func TestLiquidationRecoversWhenSalesCoverTheLoan(t *testing.T) {
	m := newMarketWith(t, tenTicksADay)
	m.fund("lena", 100_000, 0)
	m.fund("tom", 10_000, 0)
	m.fund("sam", 0, 100)
	m.fund("bea", 100_000, 0)
	must(t, m.l.Lend(ctx, "lena", 100_000))
	must(t, m.placeLeveraged(ioc(buy("b1", "tom", 50, 1000)), 5))
	must(t, m.place(sell("s1", "sam", 50, 1000)))
	m.tick()

	// A dip to 900 breaches the margin, but liquidation sells at 950.
	must(t, m.place(sell("s2", "sam", 1, 900)))
	must(t, m.place(buy("b2", "bea", 1, 900)))
	m.tick()
	if tom := m.account("tom"); tom.Status != StatusLiquidating {
		t.Fatalf("tom = %+v", tom)
	}
	m.liquidate()
	must(t, m.place(buy("b3", "bea", 50, 950)))
	m.tick()
	tom := m.account("tom")
	if tom.Status != StatusActive || tom.Owed != 0 || tom.Cash <= 0 || tom.Cash >= 10_000 {
		t.Fatalf("tom = %+v, want active with what was left", tom)
	}
	if len(m.l.Bankruptcies()) != 0 {
		t.Fatal("no bankruptcy expected")
	}
	m.conserved(210_000)
}

func TestPoolRestoresFromStore(t *testing.T) {
	store := newMemStore()
	cfg := Config{Store: store, Prices: map[string]int64{"ACME": 1000}, Leverage: unlimited(Leverage{TicksPerDay: 10})}
	l, err := Open(ctx, cfg)
	must(t, err)
	for id, cash := range map[string]int64{"lena": 100_000, "tom": 10_000} {
		must(t, l.OpenAccount(ctx, id))
		must(t, l.Deposit(ctx, id, cash, ""))
	}
	must(t, l.OpenAccount(ctx, "sam"))
	must(t, l.DepositShares(ctx, "sam", "ACME", 30, ""))
	must(t, l.Lend(ctx, "lena", 60_000))
	must(t, l.ReserveLeveraged(ctx, buy("b1", "tom", 20, 1000), 2))
	must(t, l.Reserve(ctx, sell("s1", "sam", 10, 900)))
	exec := engine.Execution{Symbol: "ACME", BuyOrderID: "b1", SellOrderID: "s1", BuyAccountID: "tom", SellAccountID: "sam", Price: 900, Quantity: 10}
	must(t, l.ApplyTick(ctx, engine.TickResult{Tick: 1, Books: []engine.BookResult{{Symbol: "ACME", ClearingPrice: 900, Volume: 10, Executions: []engine.Execution{exec}}}}))

	r, err := Open(ctx, cfg)
	must(t, err)
	for _, id := range []string{"lena", "tom", "sam"} {
		want, _ := l.Account(id)
		got, _ := r.Account(id)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("restored %s = %+v, want %+v", id, got, want)
		}
	}
	if !reflect.DeepEqual(r.Pool(), l.Pool()) || !reflect.DeepEqual(r.holds, l.holds) || r.marks["ACME"] != 900 {
		t.Errorf("restored pool %+v, want %+v; marks %v", r.Pool(), l.Pool(), r.marks)
	}
}

func TestLoanLimits(t *testing.T) {
	m := newMarket(t) // defaults: 5% of the pool per trade, 50% per trader
	m.fund("lena", 100_000, 0)
	m.fund("tom", 100_000, 0)
	m.fund("sam", 0, 1_000)
	must(t, m.l.Lend(ctx, "lena", 100_000))
	if p := m.l.Pool(); p.MaxTradeLoan != 5_000 || p.MaxTraderLoan != 50_000 || p.MaxTradeLoanBps != 500 || p.MaxTraderLoanBps != 5_000 {
		t.Fatalf("pool = %+v", p)
	}

	// 2x 11 @ 1000 borrows 5,500: over the per-trade limit.
	if err := m.l.ReserveLeveraged(ctx, buy("big", "tom", 11, 1000), 2); !errors.Is(err, ErrLoanLimit) {
		t.Fatalf("over the trade limit: %v", err)
	}
	// Nine 5,000 loans, five filled and four still open, plus interest
	// leave tom just under 5,000 short of the per-trader limit.
	for i := range 9 {
		must(t, m.placeLeveraged(ioc(buy(fmt.Sprintf("b%d", i), "tom", 10, 1000)), 2))
		if i == 4 {
			must(t, m.place(sell("s1", "sam", 50, 1000)))
			m.tick()
		}
	}
	if err := m.l.ReserveLeveraged(ctx, buy("one-more", "tom", 10, 1000), 2); !errors.Is(err, ErrLoanLimit) {
		t.Fatalf("over the trader limit: %v", err)
	}
	if err := m.l.Reserve(ctx, buy("unlevered", "tom", 1, 1000)); err != nil {
		t.Fatalf("a buy without leverage is not limited: %v", err)
	}
}
