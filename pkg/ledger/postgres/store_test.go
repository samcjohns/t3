package postgres

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/samcjohns/t3/internal/pgtest"
	enginepg "github.com/samcjohns/t3/pkg/engine/postgres"

	"github.com/samcjohns/t3/pkg/engine"
	"github.com/samcjohns/t3/pkg/ledger"
)

var ctx = context.Background()

func openLedger(t *testing.T, pool *pgxpool.Pool) (*ledger.Ledger, *Store) {
	t.Helper()
	s, err := NewStore(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	l, err := ledger.Open(ctx, ledger.Config{Store: s})
	if err != nil {
		t.Fatal(err)
	}
	return l, s
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// TestTradingSurvivesRestart runs random trading through a journaled engine
// and a persisted ledger, restarting both midway and at the end, and checks
// that restored state matches and the audit trail balances.
func TestTradingSurvivesRestart(t *testing.T) {
	pool := pgtest.New(t)
	journal, err := enginepg.NewJournal(ctx, pool)
	must(t, err)

	var l *ledger.Ledger
	var store *Store
	var eng *engine.Orchestrator
	start := func() {
		l, store = openLedger(t, pool)
		eng, err = engine.OpenOrchestrator(ctx, engine.FBAMatcher{}, engine.Config{Journal: journal})
		must(t, err)
		// Recover exactly as the server does.
		must(t, l.CatchUp(ctx, eng))
	}
	start()

	accts := []string{"a", "b", "c", "d"}
	for _, a := range accts {
		must(t, l.OpenAccount(ctx, a))
		must(t, l.Deposit(ctx, a, 1_000_000, "starting-cash"))
		must(t, l.DepositShares(ctx, a, "ACME", 500, "seed"))
	}

	rng := rand.New(rand.NewPCG(5, 6))
	id := 0
	for round := range 60 {
		for range 1 + rng.IntN(6) {
			id++
			o := engine.Order{ID: fmt.Sprint(id), AccountID: accts[rng.IntN(len(accts))], Symbol: "ACME",
				Type: engine.Limit, Quantity: 1 + rng.Int64N(40), LimitPrice: 950 + rng.Int64N(100)}
			o.Direction = engine.Buy
			if rng.IntN(2) == 0 {
				o.Direction = engine.Sell
			}
			if err := l.Reserve(ctx, o); err != nil {
				if errors.Is(err, ledger.ErrInsufficientFunds) || errors.Is(err, ledger.ErrInsufficientShares) {
					continue
				}
				t.Fatal(err)
			}
			_, err := eng.Submit(ctx, o)
			must(t, err)
		}
		tr, err := eng.Tick(ctx)
		must(t, err)
		if round%3 == 0 {
			// Simulate the ledger missing a tick (e.g. a crash before it
			// applied); the restart must catch it up.
			continue
		}
		// Deliver as the server's tick callback does: catch up on a gap.
		if err := l.ApplyTick(ctx, tr); errors.Is(err, ledger.ErrTickOutOfOrder) {
			must(t, l.CatchUp(ctx, eng))
		} else {
			must(t, err)
		}

		if round == 30 {
			start()
		}
	}

	start()
	if l.LastTick() != eng.LastTick() {
		t.Fatalf("ledger at tick %d, engine at %d", l.LastTick(), eng.LastTick())
	}
	if eng.LastTick() != 60 {
		t.Fatalf("engine at tick %d", eng.LastTick())
	}
	must(t, store.Audit(ctx))

	var totalCash, totalShares int64
	for _, a := range accts {
		acc, err := l.Account(a)
		must(t, err)
		totalCash += acc.Cash
		for _, h := range acc.Holdings {
			totalShares += h.Quantity
		}
	}
	if totalCash != 4_000_000 || totalShares != 2000 {
		t.Fatalf("conservation broken: cash %d shares %d", totalCash, totalShares)
	}
}

func TestRestoreMatches(t *testing.T) {
	pool := pgtest.New(t)
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	s, err := NewStore(ctx, pool)
	must(t, err)
	l, err := ledger.Open(ctx, ledger.Config{Store: s, Clock: func() time.Time { return now }})
	must(t, err)
	must(t, l.OpenAccount(ctx, "alice"))
	must(t, l.OpenAccount(ctx, "bob"))
	must(t, l.Deposit(ctx, "alice", 100_100, "starting-cash"))
	must(t, l.Withdraw(ctx, "alice", 100, ""))
	must(t, l.DepositShares(ctx, "bob", "ACME", 30, ""))
	buy := engine.Order{ID: "b1", AccountID: "alice", Symbol: "ACME", Direction: engine.Buy, Type: engine.Limit, Quantity: 20, LimitPrice: 1010}
	sell := engine.Order{ID: "s1", AccountID: "bob", Symbol: "ACME", Direction: engine.Sell, Type: engine.Limit, Quantity: 30, LimitPrice: 1000}
	must(t, l.Reserve(ctx, buy))
	must(t, l.Reserve(ctx, sell))
	exec := engine.Execution{Symbol: "ACME", BuyOrderID: "b1", SellOrderID: "s1", BuyAccountID: "alice", SellAccountID: "bob", Price: 1005, Quantity: 20}
	must(t, l.ApplyTick(ctx, engine.TickResult{Tick: 1, Books: []engine.BookResult{{Symbol: "ACME", Executions: []engine.Execution{exec}}}}))

	r, _ := openLedger(t, pool)
	for _, id := range []string{"alice", "bob"} {
		want, _ := l.Account(id)
		got, _ := r.Account(id)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("restored %s = %+v, want %+v", id, got, want)
		}
	}
	// bob's remaining hold (10 shares) survives; alice's filled buy is gone.
	if err := r.Release(ctx, "s1"); err != nil {
		t.Errorf("restored hold s1: %v", err)
	}
	if err := r.Release(ctx, "b1"); !errors.Is(err, ledger.ErrUnknownHold) {
		t.Errorf("filled hold b1 restored: %v", err)
	}
	must(t, r.Deposit(ctx, "alice", 5, "starting-cash"))
	if a, _ := r.Account("alice"); a.Cash != 79_900 {
		t.Errorf("reference not restored: cash %d", a.Cash)
	}
	must(t, s.Audit(ctx))
}

func TestDatabaseRejectsDoubleApply(t *testing.T) {
	pool := pgtest.New(t)
	l1, _ := openLedger(t, pool)
	l2, _ := openLedger(t, pool) // a second writer on the same schema
	must(t, l1.ApplyTick(ctx, engine.TickResult{Tick: 1}))
	if err := l2.ApplyTick(ctx, engine.TickResult{Tick: 1}); !errors.Is(err, ledger.ErrTickOutOfOrder) {
		t.Fatalf("second writer applying tick 1: %v", err)
	}
}

func TestAuditDetectsTampering(t *testing.T) {
	pool := pgtest.New(t)
	l, s := openLedger(t, pool)
	must(t, l.OpenAccount(ctx, "a"))
	must(t, l.Deposit(ctx, "a", 100, ""))
	must(t, s.Audit(ctx))
	if _, err := pool.Exec(ctx, `UPDATE ledger.accounts SET cash = 1000 WHERE id = 'a'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Audit(ctx); err == nil {
		t.Fatal("audit missed a tampered balance")
	}
}

// TestLeverageSurvivesRestart borrows, accrues interest and goes bankrupt,
// then checks a restored ledger matches and the audit balances.
func TestLeverageSurvivesRestart(t *testing.T) {
	pool := pgtest.New(t)
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	s, err := NewStore(ctx, pool)
	must(t, err)
	cfg := ledger.Config{Store: s, Clock: func() time.Time { return now }, Leverage: ledger.Leverage{TicksPerDay: 10, MaxTradeLoan: 10_000, MaxTraderLoan: 10_000}}
	l, err := ledger.Open(ctx, cfg)
	must(t, err)
	for id, cash := range map[string]int64{"lena": 100_000, "tom": 10_000, "bea": 100_000} {
		must(t, l.OpenAccount(ctx, id))
		must(t, l.Deposit(ctx, id, cash, ""))
	}
	must(t, l.OpenAccount(ctx, "sam"))
	must(t, l.DepositShares(ctx, "sam", "ACME", 100, ""))
	must(t, l.Lend(ctx, "lena", 100_000))

	order := func(id, acct string, dir engine.Direction, qty, price int64) engine.Order {
		return engine.Order{ID: id, AccountID: acct, Symbol: "ACME", Direction: dir, Type: engine.Limit, Quantity: qty, LimitPrice: price}
	}
	exec := func(buy, sell engine.Order, qty, price int64) engine.Execution {
		return engine.Execution{Symbol: "ACME", BuyOrderID: buy.ID, SellOrderID: sell.ID, BuyAccountID: buy.AccountID, SellAccountID: sell.AccountID, Price: price, Quantity: qty}
	}
	tick := func(n uint64, price int64, execs ...engine.Execution) {
		t.Helper()
		var vol int64
		for _, e := range execs {
			vol += e.Quantity
		}
		must(t, l.ApplyTick(ctx, engine.TickResult{Tick: n, Books: []engine.BookResult{{Symbol: "ACME", ClearingPrice: price, Volume: vol, Executions: execs}}}))
	}

	b1, s1 := order("b1", "tom", engine.Buy, 50, 1000), order("s1", "sam", engine.Sell, 50, 1000)
	must(t, l.ReserveLeveraged(ctx, b1, 5))
	must(t, l.Reserve(ctx, s1))
	tick(1, 1000, exec(b1, s1, 50, 1000))

	b2, s2 := order("b2", "bea", engine.Buy, 10, 700), order("s2", "sam", engine.Sell, 10, 700)
	must(t, l.Reserve(ctx, b2))
	must(t, l.Reserve(ctx, s2))
	tick(2, 700, exec(b2, s2, 10, 700)) // tom's equity goes negative
	liq, err := l.Liquidations(ctx)
	must(t, err)
	if len(liq.Sell) != 1 {
		t.Fatalf("liquidation = %+v", liq)
	}
	b3 := order("b3", "bea", engine.Buy, 50, 600)
	must(t, l.Reserve(ctx, b3))
	tick(3, 600, exec(b3, liq.Sell[0], 50, 600))
	if a, _ := l.Account("tom"); a.Status != ledger.StatusBankrupt {
		t.Fatalf("tom = %+v", a)
	}
	must(t, l.SetBlocked(ctx, "sam", true))

	r, err := ledger.Open(ctx, cfg)
	must(t, err)
	for _, id := range []string{"lena", "tom", "sam", "bea"} {
		want, _ := l.Account(id)
		got, _ := r.Account(id)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("restored %s = %+v, want %+v", id, got, want)
		}
	}
	if !reflect.DeepEqual(r.Pool(), l.Pool()) {
		t.Errorf("restored pool = %+v, want %+v", r.Pool(), l.Pool())
	}
	if got, want := r.Bankruptcies(), l.Bankruptcies(); !reflect.DeepEqual(got, want) {
		t.Errorf("restored bankruptcies = %+v, want %+v", got, want)
	}
	must(t, s.Audit(ctx))
}
