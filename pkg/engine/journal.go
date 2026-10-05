package engine

import (
	"context"
	"errors"
)

// OrderStatus is an order's lifecycle state as recorded by a Journal.
type OrderStatus string

const (
	// StatusPending orders are accepted but have not been in a tick yet.
	StatusPending OrderStatus = "pending"
	// StatusResting orders have been in a tick and have quantity left.
	StatusResting OrderStatus = "resting"
	StatusFilled  OrderStatus = "filled"
	StatusExpired OrderStatus = "expired"
)

// OrderUpdate records an order's state after a tick.
type OrderUpdate struct {
	ID        string
	Remaining int64
	Status    OrderStatus
}

// Snapshot is the engine state a Journal restores on startup.
type Snapshot struct {
	// Pending and Resting are in sequence order. A resting order's Quantity
	// is its remaining quantity.
	Pending      []Order
	Resting      []Order
	LastTick     uint64
	LastSequence uint64
}

// Journal durably records engine state. Every method must have persisted its
// effect before returning nil.
type Journal interface {
	Load(ctx context.Context) (Snapshot, error)
	// AcceptOrder records a new pending order. It returns an error wrapping
	// ErrDuplicateOrderID if the ID has been used by an order the journal
	// still holds. Compaction may forget orders that expired unfilled.
	AcceptOrder(ctx context.Context, o Order) error
	// CommitTick atomically records a tick's result and the resulting state
	// of every order whose state changed.
	CommitTick(ctx context.Context, tr TickResult, updates []OrderUpdate) error
	// TicksAfter returns up to limit committed ticks after the given tick, in
	// order.
	TicksAfter(ctx context.Context, after uint64, limit int) ([]TickResult, error)
	// KnownOrders reports which of ids have been accepted, excluding orders
	// that compaction has forgotten.
	KnownOrders(ctx context.Context, ids []string) (map[string]bool, error)
}

// ErrNoJournal is returned by history queries on an orchestrator that runs
// without a Journal.
var ErrNoJournal = errors.New("engine has no journal")
