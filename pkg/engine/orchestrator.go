package engine

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"
)

// DefaultHeartbeat is the batch clearing interval.
const DefaultHeartbeat = 10 * time.Second

// Config configures an Orchestrator. Zero values select defaults.
type Config struct {
	// Heartbeat is the interval between ticks. Defaults to DefaultHeartbeat.
	Heartbeat time.Duration
	// OnTick is called with every tick's result, in tick order, before the
	// next tick starts. It must not block for long or call Tick.
	OnTick func(TickResult)
	// Clock supplies tick timestamps. Defaults to time.Now.
	Clock func() time.Time
}

// Orchestrator buffers incoming orders, hands each symbol's batch to the
// matcher on every heartbeat, carries resting limit orders into the next
// batch, and publishes a TickResult. It is safe for concurrent use.
type Orchestrator struct {
	matcher   BatchMatcher
	heartbeat time.Duration
	onTick    func(TickResult)
	clock     func() time.Time

	// mu guards the intake buffer and the set of live order IDs.
	mu      sync.Mutex
	pending []Order
	live    map[string]struct{}
	seq     uint64

	// tickMu serialises ticks and guards state only ticks touch.
	tickMu sync.Mutex
	books  map[string][]Order
	tick   uint64
}

func NewOrchestrator(matcher BatchMatcher, cfg Config) *Orchestrator {
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = DefaultHeartbeat
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	return &Orchestrator{
		matcher:   matcher,
		heartbeat: cfg.Heartbeat,
		onTick:    cfg.OnTick,
		clock:     cfg.Clock,
		live:      make(map[string]struct{}),
		books:     make(map[string][]Order),
	}
}

// Submit validates an order and queues it for the next tick. It returns the
// accepted order with its engine-assigned Sequence. Order IDs must be unique
// among orders that are pending or resting.
func (o *Orchestrator) Submit(order Order) (Order, error) {
	if err := order.Validate(); err != nil {
		return Order{}, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, dup := o.live[order.ID]; dup {
		return Order{}, fmt.Errorf("%w: %q", ErrDuplicateOrderID, order.ID)
	}
	o.seq++
	order.Sequence = o.seq
	o.pending = append(o.pending, order)
	o.live[order.ID] = struct{}{}
	return order, nil
}

// Run ticks every heartbeat until ctx is cancelled.
func (o *Orchestrator) Run(ctx context.Context) error {
	t := time.NewTicker(o.heartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			o.Tick()
		}
	}
}

// Tick clears the current batch immediately. Run calls it on every
// heartbeat; it is exported so hosts and tests can drive ticks directly.
func (o *Orchestrator) Tick() TickResult {
	o.tickMu.Lock()
	defer o.tickMu.Unlock()

	o.mu.Lock()
	incoming := o.pending
	o.pending = nil
	o.mu.Unlock()

	for _, ord := range incoming {
		o.books[ord.Symbol] = append(o.books[ord.Symbol], ord)
	}

	o.tick++
	result := TickResult{Tick: o.tick, Timestamp: o.clock(), Books: []BookResult{}}
	var done []string

	symbols := make([]string, 0, len(o.books))
	for sym := range o.books {
		symbols = append(symbols, sym)
	}
	slices.Sort(symbols)

	for _, sym := range symbols {
		m := o.matcher.Match(o.books[sym])
		book := BookResult{
			Symbol:          sym,
			ClearingPrice:   m.ClearingPrice,
			Volume:          m.Volume,
			Executions:      m.Executions,
			ExpiredOrderIDs: []string{},
		}
		if book.Executions == nil {
			book.Executions = []Execution{}
		}
		for _, e := range m.Expired {
			book.ExpiredOrderIDs = append(book.ExpiredOrderIDs, e.ID)
		}
		result.Books = append(result.Books, book)
		done = append(done, finishedIDs(o.books[sym], m.Resting)...)

		if len(m.Resting) == 0 {
			delete(o.books, sym)
		} else {
			o.books[sym] = m.Resting
		}
	}

	o.mu.Lock()
	for _, id := range done {
		delete(o.live, id)
	}
	o.mu.Unlock()

	if o.onTick != nil {
		o.onTick(result)
	}
	return result
}

// finishedIDs returns the IDs in batch that are no longer resting.
func finishedIDs(batch, resting []Order) []string {
	still := make(map[string]struct{}, len(resting))
	for _, r := range resting {
		still[r.ID] = struct{}{}
	}
	var out []string
	for _, b := range batch {
		if _, ok := still[b.ID]; !ok {
			out = append(out, b.ID)
		}
	}
	return out
}
