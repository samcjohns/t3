package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"
)

var ctx = context.Background()

func mustTick(t *testing.T, o *Orchestrator) TickResult {
	t.Helper()
	r, err := o.Tick(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

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
		{ID: "x", AccountID: "a", Symbol: "ACME", Direction: Buy, Type: Limit, Quantity: 1, LimitPrice: 1, TimeInForce: "FOK"},
	}
	for _, b := range bad {
		if _, err := o.Submit(ctx, b); !errors.Is(err, ErrInvalidOrder) {
			t.Errorf("Submit(%+v) err = %v, want ErrInvalidOrder", b, err)
		}
	}
}

func TestSubmitAssignsSequenceAndRejectsLiveDuplicates(t *testing.T) {
	o := NewOrchestrator(FBAMatcher{}, Config{})
	a, err := o.Submit(ctx, Order{Sequence: 999, ID: "a", AccountID: "x", Symbol: "ACME", Direction: Buy, Type: Limit, Quantity: 1, LimitPrice: 1})
	if err != nil || a.Sequence != 1 {
		t.Fatalf("got %+v, %v; want sequence 1", a, err)
	}
	if _, err := o.Submit(ctx, order("a", Sell, 1, 1)); !errors.Is(err, ErrDuplicateOrderID) {
		t.Fatalf("duplicate pending id: err = %v", err)
	}

	// After a's opposite side arrives and both fill, the id is free again.
	o.Submit(ctx, order("b", Sell, 1, 1))
	mustTick(t, o)
	if _, err := o.Submit(ctx, order("a", Buy, 1, 1)); err != nil {
		t.Fatalf("id should be reusable after fill: %v", err)
	}
}

func TestRestingOrdersCarryAcrossTicks(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	o := NewOrchestrator(FBAMatcher{}, Config{Clock: func() time.Time { return now }})

	o.Submit(ctx, order("s1", Sell, 100, 1000))
	r1 := mustTick(t, o)
	if r1.Tick != 1 || len(r1.Books) != 1 || r1.Books[0].Volume != 0 {
		t.Fatalf("tick 1 = %+v, want one idle book", r1)
	}

	o.Submit(ctx, order("b1", Buy, 40, 1000))
	r2 := mustTick(t, o)
	if r2.Tick != 2 || !r2.Timestamp.Equal(now) {
		t.Fatalf("tick 2 header = %d %v", r2.Tick, r2.Timestamp)
	}
	if b := r2.Books[0]; b.Volume != 40 || b.ClearingPrice != 1000 {
		t.Fatalf("tick 2 book = %+v, want 40 @ 1000", b)
	}

	// s1 still has 60 resting, so its id stays reserved.
	if _, err := o.Submit(ctx, order("s1", Sell, 1, 1)); !errors.Is(err, ErrDuplicateOrderID) {
		t.Fatalf("resting id reuse: err = %v", err)
	}

	o.Submit(ctx, order("b2", Buy, 60, 1000))
	if b := mustTick(t, o).Books[0]; b.Volume != 60 {
		t.Fatalf("tick 3 volume = %d, want 60", b.Volume)
	}
	if r := mustTick(t, o); len(r.Books) != 0 {
		t.Fatalf("tick 4 books = %+v, want empty", r.Books)
	}
}

func TestMarketRemainderExpires(t *testing.T) {
	o := NewOrchestrator(FBAMatcher{}, Config{})
	o.Submit(ctx, order("s1", Sell, 10, 1000))
	o.Submit(ctx, Order{ID: "m1", AccountID: "x", Symbol: "ACME", Direction: Buy, Type: Market, Quantity: 25, MaxCost: 1_000_000})
	b := mustTick(t, o).Books[0]
	if b.Volume != 10 || len(b.ExpiredOrderIDs) != 1 || b.ExpiredOrderIDs[0] != "m1" {
		t.Fatalf("book = %+v, want 10 filled and m1 expired", b)
	}
	if r := mustTick(t, o); len(r.Books) != 0 {
		t.Fatalf("expired market order should not rest: %+v", r.Books)
	}
}

func TestSymbolsClearIndependentlyInOrder(t *testing.T) {
	o := NewOrchestrator(FBAMatcher{}, Config{})
	for _, sym := range []string{"ZED", "ACME", "MID"} {
		o.Submit(ctx, Order{ID: sym + "-b", AccountID: "x", Symbol: sym, Direction: Buy, Type: Limit, Quantity: 5, LimitPrice: 100})
		o.Submit(ctx, Order{ID: sym + "-s", AccountID: "y", Symbol: sym, Direction: Sell, Type: Limit, Quantity: 5, LimitPrice: 100})
	}
	r := mustTick(t, o)
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

	o.Submit(ctx, order("b", Buy, 1, 10))
	o.Submit(ctx, order("s", Sell, 1, 10))
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
				if _, err := o.Submit(ctx, order(fmt.Sprintf("%d-%d", w, i), dir, 1, 100)); err != nil {
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
				o.Tick(ctx)
			}
		}
	})
	wg.Wait()
	close(stop)
	ticker.Wait()
	mustTick(t, o)

	// Equal buys and sells at one price always fully cross by the end.
	if want := int64(workers * perWorker / 2); volume != want {
		t.Fatalf("total volume = %d, want %d", volume, want)
	}
}

// memJournal is an in-memory Journal for testing the orchestrator's use of
// it. failNext makes the next write fail.
type memJournal struct {
	mu       sync.Mutex
	orders   map[string]Order
	status   map[string]OrderUpdate
	ticks    []TickResult
	failNext bool
}

func newMemJournal() *memJournal {
	return &memJournal{orders: map[string]Order{}, status: map[string]OrderUpdate{}}
}

var errInjected = errors.New("injected failure")

func (j *memJournal) fail() error {
	if j.failNext {
		j.failNext = false
		return errInjected
	}
	return nil
}

func (j *memJournal) Load(context.Context) (Snapshot, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	var s Snapshot
	for _, o := range j.orders {
		s.LastSequence = max(s.LastSequence, o.Sequence)
		switch u := j.status[o.ID]; u.Status {
		case StatusPending:
			s.Pending = append(s.Pending, o)
		case StatusResting:
			o.Quantity = u.Remaining
			s.Resting = append(s.Resting, o)
		}
	}
	bySeq := func(a, b Order) int { return int(a.Sequence) - int(b.Sequence) }
	slices.SortFunc(s.Pending, bySeq)
	slices.SortFunc(s.Resting, bySeq)
	if n := len(j.ticks); n > 0 {
		s.LastTick = j.ticks[n-1].Tick
	}
	return s, nil
}

func (j *memJournal) AcceptOrder(_ context.Context, o Order) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.fail(); err != nil {
		return err
	}
	if _, ok := j.orders[o.ID]; ok {
		return ErrDuplicateOrderID
	}
	j.orders[o.ID] = o
	j.status[o.ID] = OrderUpdate{o.ID, o.Quantity, StatusPending}
	return nil
}

func (j *memJournal) CommitTick(_ context.Context, tr TickResult, updates []OrderUpdate) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.fail(); err != nil {
		return err
	}
	j.ticks = append(j.ticks, tr)
	for _, u := range updates {
		j.status[u.ID] = u
	}
	return nil
}

func (j *memJournal) TicksAfter(_ context.Context, after uint64, limit int) ([]TickResult, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []TickResult
	for _, tr := range j.ticks {
		if tr.Tick > after && len(out) < limit {
			out = append(out, tr)
		}
	}
	return out, nil
}

func (j *memJournal) KnownOrders(_ context.Context, ids []string) (map[string]bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := map[string]bool{}
	for _, id := range ids {
		if _, ok := j.orders[id]; ok {
			out[id] = true
		}
	}
	return out, nil
}

func TestJournalRecordsOrderLifecycle(t *testing.T) {
	j := newMemJournal()
	o, err := OpenOrchestrator(ctx, FBAMatcher{}, Config{Journal: j})
	if err != nil {
		t.Fatal(err)
	}
	o.Submit(ctx, order("s1", Sell, 10, 1000))
	o.Submit(ctx, order("b1", Buy, 4, 1000))
	o.Submit(ctx, Order{ID: "m1", AccountID: "x", Symbol: "ACME", Direction: Buy, Type: Market, Quantity: 100, MaxCost: 1_000_000})
	o.Submit(ctx, order("idle", Buy, 1, 1))
	if got := j.status["b1"].Status; got != StatusPending {
		t.Fatalf("before tick b1 is %s", got)
	}
	mustTick(t, o)

	want := map[string]OrderUpdate{
		"s1":   {"s1", 0, StatusFilled}, // all 10 to m1, which has priority
		"b1":   {"b1", 4, StatusResting},
		"m1":   {"m1", 90, StatusExpired},
		"idle": {"idle", 1, StatusResting},
	}
	if !maps.Equal(j.status, want) {
		t.Fatalf("statuses = %v", j.status)
	}
	if ticks, _ := o.TicksAfter(ctx, 0, 10); len(ticks) != 1 || ticks[0].Tick != 1 {
		t.Fatalf("TicksAfter = %+v", ticks)
	}
	if known, _ := o.KnownOrders(ctx, []string{"s1", "ghost"}); !known["s1"] || known["ghost"] {
		t.Fatalf("KnownOrders = %v", known)
	}
}

func TestRecoverFromJournal(t *testing.T) {
	j := newMemJournal()
	o1, _ := OpenOrchestrator(ctx, FBAMatcher{}, Config{Journal: j})
	o1.Submit(ctx, order("s1", Sell, 10, 1000))
	mustTick(t, o1)                           // s1 rests
	o1.Submit(ctx, order("b1", Buy, 4, 1000)) // still pending at "crash"

	o2, err := OpenOrchestrator(ctx, FBAMatcher{}, Config{Journal: j})
	if err != nil {
		t.Fatal(err)
	}
	if o2.LastTick() != 1 {
		t.Fatalf("LastTick = %d", o2.LastTick())
	}
	if _, err := o2.Submit(ctx, order("s1", Sell, 1, 1)); !errors.Is(err, ErrDuplicateOrderID) {
		t.Fatalf("restored live IDs: %v", err)
	}
	b2, err := o2.Submit(ctx, order("b2", Buy, 6, 1000))
	if err != nil || b2.Sequence != 3 {
		t.Fatalf("sequence after restore = %d, %v", b2.Sequence, err)
	}
	r := mustTick(t, o2)
	if r.Tick != 2 || r.Books[0].Volume != 10 {
		t.Fatalf("tick after restore = %+v", r)
	}
}

func TestFailedCommitChangesNothing(t *testing.T) {
	j := newMemJournal()
	var published []uint64
	o, _ := OpenOrchestrator(ctx, FBAMatcher{}, Config{Journal: j, OnTick: func(r TickResult) { published = append(published, r.Tick) }})

	j.failNext = true
	if _, err := o.Submit(ctx, order("b1", Buy, 5, 1000)); !errors.Is(err, errInjected) {
		t.Fatalf("submit with failing journal: %v", err)
	}
	if b1, err := o.Submit(ctx, order("b1", Buy, 5, 1000)); err != nil || b1.Sequence != 1 {
		t.Fatalf("retry = %+v, %v; failed submit must not consume a sequence", b1, err)
	}
	o.Submit(ctx, order("s1", Sell, 5, 1000))

	j.failNext = true
	if _, err := o.Tick(ctx); !errors.Is(err, errInjected) {
		t.Fatalf("tick with failing journal: %v", err)
	}
	if o.LastTick() != 0 || len(published) != 0 {
		t.Fatal("failed tick must not advance or publish")
	}
	r := mustTick(t, o)
	if r.Tick != 1 || r.Books[0].Volume != 5 {
		t.Fatalf("retried tick = %+v", r)
	}
}

func TestHistoryNeedsJournal(t *testing.T) {
	o := NewOrchestrator(FBAMatcher{}, Config{})
	if _, err := o.TicksAfter(ctx, 0, 1); !errors.Is(err, ErrNoJournal) {
		t.Error(err)
	}
	if _, err := o.KnownOrders(ctx, nil); !errors.Is(err, ErrNoJournal) {
		t.Error(err)
	}
}
