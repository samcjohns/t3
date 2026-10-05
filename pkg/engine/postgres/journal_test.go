package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/samcjohns/t3/internal/pgtest"
	"github.com/samcjohns/t3/pkg/engine"
)

var ctx = context.Background()

func limit(id string, dir engine.Direction, qty, price int64) engine.Order {
	return engine.Order{ID: id, AccountID: "acct-" + id, Symbol: "ACME", Direction: dir, Type: engine.Limit, Quantity: qty, LimitPrice: price}
}

func open(t *testing.T, pool *pgxpool.Pool, onTick func(engine.TickResult)) *engine.Orchestrator {
	t.Helper()
	j, err := NewJournal(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	clock := func() time.Time { return time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC) }
	o, err := engine.OpenOrchestrator(ctx, engine.FBAMatcher{}, engine.Config{Journal: j, Clock: clock, OnTick: onTick})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func submit(t *testing.T, o *engine.Orchestrator, ord engine.Order) {
	t.Helper()
	if _, err := o.Submit(ctx, ord); err != nil {
		t.Fatal(err)
	}
}

func tick(t *testing.T, o *engine.Orchestrator) engine.TickResult {
	t.Helper()
	r, err := o.Tick(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRestartRestoresBookAndHistory(t *testing.T) {
	pool, url := pgtest.NewURL(t)
	o := open(t, pool, nil)

	submit(t, o, limit("s1", engine.Sell, 10, 1000))
	submit(t, o, limit("b1", engine.Buy, 4, 1000))
	submit(t, o, engine.Order{ID: "m1", AccountID: "x", Symbol: "ACME", Direction: engine.Buy, Type: engine.Market, Quantity: 1, MaxCost: 5})
	first := tick(t, o)                            // b1 fills 4 against s1; m1 cannot afford a share and expires
	submit(t, o, limit("b2", engine.Buy, 2, 1000)) // pending at "crash"
	ioc := limit("ioc", engine.Buy, 1, 1)
	ioc.TimeInForce = engine.IOC
	submit(t, o, ioc) // pending IOC must still expire after restart

	// Restart against the same database with a fresh pool and orchestrator.
	pool.Close()
	pool2, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool2.Close)
	o2 := open(t, pool2, nil)

	if o2.LastTick() != 1 {
		t.Fatalf("LastTick = %d", o2.LastTick())
	}
	for _, id := range []string{"s1", "b2", "m1", "b1"} {
		_, err := o2.Submit(ctx, limit(id, engine.Buy, 1, 1))
		if !errors.Is(err, engine.ErrDuplicateOrderID) {
			t.Errorf("reusing %s after restart: %v", id, err)
		}
	}
	submit(t, o2, limit("b3", engine.Buy, 10, 1000))
	r := tick(t, o2)
	// s1 has 6 left; b2 (pending, earlier sequence) takes 2, b3 takes 4.
	if r.Tick != 2 || r.Books[0].Volume != 6 {
		t.Fatalf("tick after restart = %+v", r)
	}
	if exp := r.Books[0].ExpiredOrderIDs; len(exp) != 1 || exp[0] != "ioc" {
		t.Fatalf("restored IOC did not expire: %v", exp)
	}

	ticks, err := o2.TicksAfter(ctx, 0, 10)
	if err != nil || len(ticks) != 2 {
		t.Fatalf("TicksAfter = %d ticks, %v", len(ticks), err)
	}
	if got := ticks[0]; got.Tick != 1 || !got.Timestamp.Equal(first.Timestamp) ||
		len(got.Books[0].Executions) != 1 || got.Books[0].ExpiredOrderIDs[0] != "m1" {
		t.Fatalf("stored tick 1 = %+v", got)
	}
	if later, _ := o2.TicksAfter(ctx, 1, 10); len(later) != 1 || later[0].Tick != 2 {
		t.Fatalf("TicksAfter(1) = %+v", later)
	}

	known, err := o2.KnownOrders(ctx, []string{"s1", "m1", "ghost"})
	if err != nil || !known["s1"] || !known["m1"] || known["ghost"] {
		t.Fatalf("KnownOrders = %v, %v", known, err)
	}

	var status string
	var remaining int64
	var closed *int64
	if err := pool2.QueryRow(ctx, `SELECT status, remaining, closed_tick FROM engine.orders WHERE id = 'b3'`).Scan(&status, &remaining, &closed); err != nil {
		t.Fatal(err)
	}
	if status != "resting" || remaining != 6 || closed != nil {
		t.Fatalf("b3 = %s %d %v", status, remaining, closed)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	pool := pgtest.New(t)
	for range 2 {
		if _, err := NewJournal(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEmptyTickIsRecorded(t *testing.T) {
	o := open(t, pgtest.New(t), nil)
	tick(t, o)
	tick(t, o)
	if ticks, _ := o.TicksAfter(ctx, 0, 10); len(ticks) != 2 {
		t.Fatalf("got %d ticks, want 2", len(ticks))
	}
}
