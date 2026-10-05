// Package engine is the pure domain core of the market: order types, the
// frequent batch auction matcher, and the tick orchestrator. It has no
// transport, persistence, or framework dependencies.
package engine

import (
	"errors"
	"fmt"
	"time"
)

// Direction is the side of an order.
type Direction string

const (
	Buy  Direction = "BUY"
	Sell Direction = "SELL"
)

// OrderType determines how an order is priced.
type OrderType string

const (
	// Limit orders trade at their limit price or better and rest in the book
	// across ticks until filled.
	Limit OrderType = "LIMIT"
	// Market orders trade at any clearing price and expire at the end of the
	// tick they were submitted in if not fully filled.
	Market OrderType = "MARKET"
)

// TimeInForce determines how long a limit order stays in the book.
type TimeInForce string

const (
	// GTC (good till cancelled) limit orders rest across ticks until
	// filled. It is the default when TimeInForce is empty.
	GTC TimeInForce = "GTC"
	// IOC (immediate or cancel) limit orders take part in one auction; any
	// unfilled quantity expires, as for market orders.
	IOC TimeInForce = "IOC"
)

// Prices are integer minor units (e.g. cents) and quantities are whole
// shares, so matching never touches floating point.

// Order is an instruction to buy or sell a quantity of one symbol.
type Order struct {
	ID         string    `json:"id"`
	AccountID  string    `json:"account_id"`
	Symbol     string    `json:"symbol"`
	Direction  Direction `json:"direction"`
	Type       OrderType `json:"type"`
	Quantity   int64     `json:"quantity"`
	LimitPrice int64     `json:"limit_price"`
	// MaxCost caps the total cost of a market buy: it fills at most
	// MaxCost / clearing price shares. Required for market buys and 0 for
	// every other order.
	MaxCost int64 `json:"max_cost"`
	// TimeInForce applies to limit orders; market orders always expire
	// after one auction. Empty means GTC.
	TimeInForce TimeInForce `json:"time_in_force,omitempty"`
	// Sequence is assigned by the engine on acceptance and is the time
	// priority tie-breaker. Any value supplied by the caller is overwritten.
	Sequence uint64 `json:"sequence"`
}

// Execution is a fill between one buy order and one sell order.
type Execution struct {
	Symbol        string `json:"symbol"`
	BuyOrderID    string `json:"buy_order_id"`
	SellOrderID   string `json:"sell_order_id"`
	BuyAccountID  string `json:"buy_account_id"`
	SellAccountID string `json:"sell_account_id"`
	Price         int64  `json:"price"`
	Quantity      int64  `json:"quantity"`
}

// BookResult is the outcome of one symbol's batch auction in a tick.
type BookResult struct {
	Symbol string `json:"symbol"`
	// ClearingPrice is the uniform price of every execution, or 0 when
	// Volume is 0.
	ClearingPrice int64       `json:"clearing_price"`
	Volume        int64       `json:"volume"`
	Executions    []Execution `json:"executions"`
	// ExpiredOrderIDs lists market orders whose unfilled remainder was
	// cancelled at the end of this tick.
	ExpiredOrderIDs []string `json:"expired_order_ids"`
}

// TickResult is published once per heartbeat.
type TickResult struct {
	Tick      uint64    `json:"tick"`
	Timestamp time.Time `json:"timestamp"`
	// Books holds one entry per symbol that had orders in the batch,
	// sorted by symbol. Once a tick is compacted, its stored result keeps
	// only the books that traded, with ExpiredOrderIDs emptied.
	Books []BookResult `json:"books"`
}

// MatchResult is the matcher's outcome for a single symbol's batch.
type MatchResult struct {
	ClearingPrice int64
	Volume        int64
	Executions    []Execution
	// Resting holds limit orders with unfilled quantity, with Quantity set
	// to the remainder.
	Resting []Order
	// Expired holds market orders with unfilled quantity, with Quantity set
	// to the remainder.
	Expired []Order
}

// BatchMatcher clears one symbol's batch of orders. Implementations must be
// deterministic: the same set of orders always yields the same result,
// regardless of slice order. Input orders must already be valid, share one
// symbol, and have unique sequences; the slice must not be mutated.
type BatchMatcher interface {
	Match(orders []Order) MatchResult
}

var (
	ErrInvalidOrder     = errors.New("invalid order")
	ErrDuplicateOrderID = errors.New("duplicate order id")
)

// Validate reports whether the order is well-formed. Errors wrap
// ErrInvalidOrder.
func (o Order) Validate() error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidOrder, fmt.Sprintf(format, args...))
	}
	switch {
	case o.ID == "":
		return invalid("id is required")
	case o.AccountID == "":
		return invalid("account_id is required")
	case o.Symbol == "":
		return invalid("symbol is required")
	case o.Direction != Buy && o.Direction != Sell:
		return invalid("direction must be %q or %q", Buy, Sell)
	case o.Quantity <= 0:
		return invalid("quantity must be positive")
	}
	switch o.Type {
	case Limit:
		if o.LimitPrice <= 0 {
			return invalid("limit_price must be positive for %s orders", Limit)
		}
	case Market:
		if o.LimitPrice != 0 {
			return invalid("limit_price must be 0 for %s orders", Market)
		}
	default:
		return invalid("type must be %q or %q", Limit, Market)
	}
	if o.TimeInForce != "" && o.TimeInForce != GTC && o.TimeInForce != IOC {
		return invalid("time_in_force must be %q or %q", GTC, IOC)
	}
	marketBuy := o.Type == Market && o.Direction == Buy
	switch {
	case marketBuy && o.MaxCost <= 0:
		return invalid("max_cost must be positive for %s %s orders", Market, Buy)
	case !marketBuy && o.MaxCost != 0:
		return invalid("max_cost must be 0 except for %s %s orders", Market, Buy)
	}
	return nil
}
