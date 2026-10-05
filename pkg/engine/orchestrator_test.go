package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func order(id string, dir Direction, qty, price int64) Order {
	return Order{ID: id, AccountID: "acct-" + id, Symbol: "ACME", Direction: dir, Type: Limit, Quantity: qty, LimitPrice: price}
}

func TestSubmitValidation(t *testing.T) {
	o := NewOrchestrator(FBAMatcher{}, Config{})
	bad := []Order{
		{},
		{ID: "x", AccountID: "a", Symbol: "ACME", Direction: "HOLD", Type: Limit, Quantity: 1, LimitPrice: 1},
		{ID: "x", AccountID: "a", Symbol: "ACME", Direction: Buy, Type: Limit, Quantity: 0, LimitPrice: 1},
		{ID: "x", AccountID: "a", Symbol: "ACME", Direction: Buy, Type: Limit, Quantity: 1},
		{ID: "x", AccountID: "a", Symbol: "ACME", Direction: Buy, Type: Market, Quantity: 1, LimitPrice: 5},
		{ID: "x", AccountID: "a", Symbol: "ACME", Direction: Buy, Type: "STOP", Quantity: 1},
		{ID: "x", AccountID: "a", Symbol: "ACME", Direction: Buy, Type: Market, Quantity: 1},
		{ID: "x", AccountID: "a", Symbol: "ACME", Direction: Sell, Type: Market, Quantity: 1, MaxCost: 5},
		{ID: "x", AccountID: "a", Symbol: "ACME", Direction: Buy, Type: Limit, Quantity: 1, LimitPrice: 1, MaxCost: 5},
	}
	for _, b := range bad {
		if _, err := o.Submit(b); !errors.Is(err, ErrInvalidOrder) {
			t.Errorf("Submit(%+v) err = %v, want ErrInvalidOrder", b, err)
		}
	}
}

func TestSubmitAssignsSequenceAndRejectsLiveDuplicates(t *testing.T) {
	o := NewOrchestrator(FBAMatcher{}, Config{})
	a, err := o.Submit(Order{Sequence: 999, ID: "a", AccountID: "x", Symbol: "ACME", Direction: Buy, Type: Limit, Quantity: 1, LimitPrice: 1})
	if err != nil || a.Sequence != 1 {
		t.Fatalf("got %+v, %v; want sequence 1", a, err)
	}
	if _, err := o.Submit(order("a", Sell, 1, 1)); !errors.Is(err, ErrDuplicateOrderID) {
		t.Fatalf("duplicate pending id: err = %v", err)
	}

	// After a's opposite side arrives and both fill, the id is free again.
	o.Submit(order("b", Sell, 1, 1))
	o.Tick()
	if _, err := o.Submit(order("a", Buy, 1, 1)); err != nil {
		t.Fatalf("id should be reusable after fill: %v", err)
	}
}

func TestRestingOrdersCarryAcrossTicks(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	o := NewOrchestrator(FBAMatcher{}, Config{Clock: func() time.Time { return now }})

	o.Submit(order("s1", Sell, 100, 1000))
	r1 := o.Tick()
	if r1.Tick != 1 || len(r1.Books) != 1 || r1.Books[0].Volume != 0 {
		t.Fatalf("tick 1 = %+v, want one idle book", r1)
	}

	o.Submit(order("b1", Buy, 40, 1000))
	r2 := o.Tick()
	if r2.Tick != 2 || !r2.Timestamp.Equal(now) {
		t.Fatalf("tick 2 header = %d %v", r2.Tick, r2.Timestamp)
	}
	if b := r2.Books[0]; b.Volume != 40 || b.ClearingPrice != 1000 {
		t.Fatalf("tick 2 book = %+v, want 40 @ 1000", b)
	}

	// s1 still has 60 resting, so its id stays reserved.
	if _, err := o.Submit(order("s1", Sell, 1, 1)); !errors.Is(err, ErrDuplicateOrderID) {
		t.Fatalf("resting id reuse: err = %v", err)
	}

	o.Submit(order("b2", Buy, 60, 1000))
	if b := o.Tick().Books[0]; b.Volume != 60 {
		t.Fatalf("tick 3 volume = %d, want 60", b.Volume)
	}
	if r := o.Tick(); len(r.Books) != 0 {
		t.Fatalf("tick 4 books = %+v, want empty", r.Books)
	}
}

func TestMarketRemainderExpires(t *testing.T) {
	o := NewOrchestrator(FBAMatcher{}, Config{})
	o.Submit(order("s1", Sell, 10, 1000))
	o.Submit(Order{ID: "m1", AccountID: "x", Symbol: "ACME", Direction: Buy, Type: Market, Quantity: 25, MaxCost: 1_000_000})
	b := o.Tick().Books[0]
	if b.Volume != 10 || len(b.ExpiredOrderIDs) != 1 || b.ExpiredOrderIDs[0] != "m1" {
		t.Fatalf("book = %+v, want 10 filled and m1 expired", b)
	}
	if r := o.Tick(); len(r.Books) != 0 {
		t.Fatalf("expired market order should not rest: %+v", r.Books)
	}
}

func TestSymbolsClearIndependentlyInOrder(t *testing.T) {
	o := NewOrchestrator(FBAMatcher{}, Config{})
	for _, sym := range []string{"ZED", "ACME", "MID"} {
		o.Submit(Order{ID: sym + "-b", AccountID: "x", Symbol: sym, Direction: Buy, Type: Limit, Quantity: 5, LimitPrice: 100})
		o.Submit(Order{ID: sym + "-s", AccountID: "y", Symbol: sym, Direction: Sell, Type: Limit, Quantity: 5, LimitPrice: 100})
	}
	r := o.Tick()
	var got []string
	for _, b := range r.Books {
		got = append(got, b.Symbol)
		if b.Volume != 5 || b.Executions[0].Symbol != b.Symbol {
			t.Errorf("book %+v", b)
		}
	}
	if fmt.Sprint(got) != "[ACME MID ZED]" {
		t.Fatalf("book order = %v", got)
	}
}

func TestRunTicksOnHeartbeatAndStops(t *testing.T) {
	ticks := make(chan TickResult, 10)
	o := NewOrchestrator(FBAMatcher{}, Config{
		Heartbeat: 5 * time.Millisecond,
		OnTick:    func(r TickResult) { ticks <- r },
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- o.Run(ctx) }()

	o.Submit(order("b", Buy, 1, 10))
	o.Submit(order("s", Sell, 1, 10))
	deadline := time.After(2 * time.Second)
	for filled := false; !filled; {
		select {
		case r := <-ticks:
			filled = len(r.Books) == 1 && r.Books[0].Volume == 1
		case <-deadline:
			t.Fatal("no tick cleared the batch")
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v", err)
	}
}

// TestConcurrentSubmitAndTick is meaningful under `go test -race`.
func TestConcurrentSubmitAndTick(t *testing.T) {
	var mu sync.Mutex
	var volume int64
	o := NewOrchestrator(FBAMatcher{}, Config{OnTick: func(r TickResult) {
		mu.Lock()
		defer mu.Unlock()
		for _, b := range r.Books {
			volume += b.Volume
		}
	}})

	const workers, perWorker = 8, 200
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for i := range perWorker {
				dir := Buy
				if (w+i)%2 == 0 {
					dir = Sell
				}
				if _, err := o.Submit(order(fmt.Sprintf("%d-%d", w, i), dir, 1, 100)); err != nil {
					t.Error(err)
				}
			}
		})
	}
	stop := make(chan struct{})
	ticker := sync.WaitGroup{}
	ticker.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				o.Tick()
			}
		}
	})
	wg.Wait()
	close(stop)
	ticker.Wait()
	o.Tick()

	// Equal buys and sells at one price always fully cross by the end.
	if want := int64(workers * perWorker / 2); volume != want {
		t.Fatalf("total volume = %d, want %d", volume, want)
	}
}
