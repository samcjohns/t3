package engine

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultHeartbeat is the batch clearing interval.
const DefaultHeartbeat = 10 * time.Second

// Config configures an Orchestrator. Zero values select defaults.
type Config struct {
	// Heartbeat is the interval between ticks. Defaults to DefaultHeartbeat.
	Heartbeat time.Duration
	// OnTick is called with every committed tick's result, in tick order,
	// before the next tick starts. It must not block for long or call Tick.
	OnTick func(TickResult)
	// OnTickError is called by Run when a tick fails to commit. The batch is
	// retried on the next heartbeat.
	OnTickError func(error)
	// Clock supplies tick timestamps. Defaults to time.Now.
	Clock func() time.Time
	// Journal persists orders and ticks. Without one, state is in memory
	// only and history queries return ErrNoJournal.
	Journal Journal
}

// Orchestrator buffers incoming orders, hands each symbol's batch to the
// matcher on every heartbeat, carries resting limit orders into the next
// batch, and publishes a TickResult. It is safe for concurrent use.
type Orchestrator struct {
	matcher   BatchMatcher
	heartbeat time.Duration
	onTick    func(TickResult)
	onErr     func(error)
	clock     func() time.Time
	journal   Journal

	// mu guards the intake buffer and the set of live order IDs.
	mu      sync.Mutex
	pending []Order
	live    map[string]struct{}
	seq     uint64

	// tickMu serialises ticks and guards books.
	tickMu sync.Mutex
	books  map[string][]Order
	tick   atomic.Uint64
}

// NewOrchestrator returns an orchestrator with empty state. If cfg.Journal is
// set, use OpenOrchestrator instead so existing state is restored.
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
		onErr:     cfg.OnTickError,
		clock:     cfg.Clock,
		journal:   cfg.Journal,
		live:      make(map[string]struct{}),
		books:     make(map[string][]Order),
	}
}

// OpenOrchestrator returns an orchestrator restored from cfg.Journal.
func OpenOrchestrator(ctx context.Context, matcher BatchMatcher, cfg Config) (*Orchestrator, error) {
	o := NewOrchestrator(matcher, cfg)
	if o.journal == nil {
		return o, nil
	}
	snap, err := o.journal.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading engine journal: %w", err)
	}
	o.pending = snap.Pending
	for _, ord := range snap.Resting {
		o.books[ord.Symbol] = append(o.books[ord.Symbol], ord)
	}
	for _, ord := range slices.Concat(snap.Pending, snap.Resting) {
		o.live[ord.ID] = struct{}{}
	}
	o.seq = snap.LastSequence
	o.tick.Store(snap.LastTick)
	return o, nil
}

// Submit validates an order, records it, and queues it for the next tick. It
// returns the accepted order with its engine-assigned Sequence. Order IDs
// must be unique among orders that are pending or resting, and with a
// Journal must never be reused.
func (o *Orchestrator) Submit(ctx context.Context, order Order) (Order, error) {
	if err := order.Validate(); err != nil {
		return Order{}, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, dup := o.live[order.ID]; dup {
		return Order{}, fmt.Errorf("%w: %q", ErrDuplicateOrderID, order.ID)
	}
	order.Sequence = o.seq + 1
	// Recording under the lock keeps sequences gap-free and in buffer order.
	if o.journal != nil {
		if err := o.journal.AcceptOrder(ctx, order); err != nil {
			return Order{}, err
		}
	}
	o.seq++
	o.pending = append(o.pending, order)
	o.live[order.ID] = struct{}{}
	return order, nil
}

// Run ticks every heartbeat until ctx is cancelled. A tick in progress always
// finishes before Run returns.
func (o *Orchestrator) Run(ctx context.Context) error {
	t := time.NewTicker(o.heartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			// Commit with a context that outlives shutdown so a tick that has
			// started is never abandoned halfway.
			if _, err := o.Tick(context.WithoutCancel(ctx)); err != nil && o.onErr != nil {
				o.onErr(err)
			}
		}
	}
}

// Tick clears the current batch immediately. Run calls it on every heartbeat;
// it is exported so hosts and tests can drive ticks directly. If the tick
// fails to commit, no state changes and the same orders are retried next tick.
func (o *Orchestrator) Tick(ctx context.Context) (TickResult, error) {
	o.tickMu.Lock()
	defer o.tickMu.Unlock()

	o.mu.Lock()
	incoming := slices.Clone(o.pending)
	o.mu.Unlock()

	books := maps.Clone(o.books)
	isNew := make(map[string]bool, len(incoming))
	for _, ord := range incoming {
		books[ord.Symbol] = append(slices.Clone(books[ord.Symbol]), ord)
		isNew[ord.ID] = true
	}

	result := TickResult{Tick: o.tick.Load() + 1, Timestamp: o.clock(), Books: []BookResult{}}
	var updates []OrderUpdate
	var done []string

	for _, sym := range slices.Sorted(maps.Keys(books)) {
		batch := books[sym]
		m := o.matcher.Match(batch)
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

		after := make(map[string]OrderUpdate, len(m.Resting)+len(m.Expired))
		for _, r := range m.Resting {
			after[r.ID] = OrderUpdate{r.ID, r.Quantity, StatusResting}
		}
		for _, e := range m.Expired {
			after[e.ID] = OrderUpdate{e.ID, e.Quantity, StatusExpired}
		}
		for _, ord := range batch {
			u, ok := after[ord.ID]
			if !ok {
				u = OrderUpdate{ord.ID, 0, StatusFilled}
			}
			if u.Status != StatusResting {
				done = append(done, ord.ID)
			}
			if isNew[ord.ID] || u.Status != StatusResting || u.Remaining != ord.Quantity {
				updates = append(updates, u)
			}
		}

		if len(m.Resting) == 0 {
			delete(books, sym)
		} else {
			books[sym] = m.Resting
		}
	}

	if o.journal != nil {
		if err := o.journal.CommitTick(ctx, result, updates); err != nil {
			return TickResult{}, fmt.Errorf("committing tick %d: %w", result.Tick, err)
		}
	}

	o.books = books
	o.tick.Store(result.Tick)
	o.mu.Lock()
	o.pending = slices.Clone(o.pending[len(incoming):])
	for _, id := range done {
		delete(o.live, id)
	}
	o.mu.Unlock()

	if o.onTick != nil {
		o.onTick(result)
	}
	return result, nil
}

// LastTick returns the number of the most recent committed tick.
func (o *Orchestrator) LastTick() uint64 {
	return o.tick.Load()
}

// TicksAfter returns up to limit committed ticks after the given tick.
func (o *Orchestrator) TicksAfter(ctx context.Context, after uint64, limit int) ([]TickResult, error) {
	if o.journal == nil {
		return nil, ErrNoJournal
	}
	return o.journal.TicksAfter(ctx, after, limit)
}

// KnownOrders reports which of ids the engine has ever accepted.
func (o *Orchestrator) KnownOrders(ctx context.Context, ids []string) (map[string]bool, error) {
	if o.journal == nil {
		return nil, ErrNoJournal
	}
	return o.journal.KnownOrders(ctx, ids)
}
