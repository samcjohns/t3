// Package ledger is the pure domain core of the ledger service: account cash
// balances and share holdings, the holds that fund open orders, and
// settlement of the market engine's tick results.
//
// Every order must be reserved before it is submitted to the engine. Holds
// are sized so that settlement can never overdraw an account:
//
//   - limit buy:  quantity × limit price in cash
//   - market buy: max_cost in cash
//   - any sell:   quantity in shares
//
// Settlement releases unused cash as orders fill: a limit buy that clears
// below its limit gets the difference back, and whatever is left of a market
// buy's hold is released when the order fills or expires.
package ledger

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"sync"

	"github.com/samcjohns/t3/pkg/engine"
)

var (
	ErrAccountExists      = errors.New("account already exists")
	ErrUnknownAccount     = errors.New("unknown account")
	ErrInvalidAmount      = errors.New("invalid amount")
	ErrInsufficientFunds  = errors.New("insufficient funds")
	ErrInsufficientShares = errors.New("insufficient shares")
	ErrDuplicateHold      = errors.New("order already has a hold")
	ErrUnknownHold        = errors.New("no hold for order")
	ErrTickAlreadyApplied = errors.New("tick already applied")
	ErrTickOutOfOrder     = errors.New("tick out of order")
	// ErrInconsistentTick means a tick result does not agree with the holds
	// the ledger placed. The tick is rejected without changing any state.
	ErrInconsistentTick = errors.New("tick inconsistent with holds")
)

// Account is a snapshot of one account's balances.
type Account struct {
	ID string `json:"id"`
	// Cash is the total cash balance in minor units, including CashHeld.
	Cash     int64 `json:"cash"`
	CashHeld int64 `json:"cash_held"`
	// Holdings is sorted by symbol and omits symbols with no shares.
	Holdings []Holding `json:"holdings"`
}

// Holding is an account's position in one symbol.
type Holding struct {
	Symbol string `json:"symbol"`
	// Quantity is the total share count, including Held.
	Quantity int64 `json:"quantity"`
	Held     int64 `json:"held"`
}

type account struct {
	cash, cashHeld     int64
	shares, sharesHeld map[string]int64
}

// hold reserves the funding for one open order.
type hold struct {
	order engine.Order
	// remaining is the order's unfilled quantity.
	remaining int64
	// cash and shares are what is still held for the order.
	cash, shares int64
}

// Ledger is safe for concurrent use.
type Ledger struct {
	mu       sync.Mutex
	accounts map[string]*account
	holds    map[string]*hold
	lastTick uint64
}

func New() *Ledger {
	return &Ledger{
		accounts: make(map[string]*account),
		holds:    make(map[string]*hold),
	}
}

func (l *Ledger) OpenAccount(id string) error {
	if id == "" {
		return fmt.Errorf("%w: account id is required", ErrInvalidAmount)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.accounts[id]; ok {
		return fmt.Errorf("%w: %q", ErrAccountExists, id)
	}
	l.accounts[id] = &account{shares: map[string]int64{}, sharesHeld: map[string]int64{}}
	return nil
}

// Deposit credits cash to an account.
func (l *Ledger) Deposit(accountID string, amount int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, err := l.account(accountID)
	if err != nil {
		return err
	}
	if amount <= 0 || a.cash > math.MaxInt64-amount {
		return fmt.Errorf("%w: deposit %d", ErrInvalidAmount, amount)
	}
	a.cash += amount
	return nil
}

// DepositShares credits shares to an account, e.g. to seed market makers.
func (l *Ledger) DepositShares(accountID, symbol string, quantity int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, err := l.account(accountID)
	if err != nil {
		return err
	}
	if symbol == "" || quantity <= 0 || a.shares[symbol] > math.MaxInt64-quantity {
		return fmt.Errorf("%w: deposit %d %q", ErrInvalidAmount, quantity, symbol)
	}
	a.shares[symbol] += quantity
	return nil
}

// Reserve places a hold funding the order. It must succeed before the order
// is submitted to the engine; if the engine then rejects the order, call
// Release.
func (l *Ledger) Reserve(order engine.Order) error {
	if err := order.Validate(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	a, err := l.account(order.AccountID)
	if err != nil {
		return err
	}
	if _, ok := l.holds[order.ID]; ok {
		return fmt.Errorf("%w: %q", ErrDuplicateHold, order.ID)
	}

	h := &hold{order: order, remaining: order.Quantity}
	switch {
	case order.Direction == engine.Sell:
		if avail := a.shares[order.Symbol] - a.sharesHeld[order.Symbol]; avail < order.Quantity {
			return fmt.Errorf("%w: need %d %s, have %d available", ErrInsufficientShares, order.Quantity, order.Symbol, avail)
		}
		h.shares = order.Quantity
		a.sharesHeld[order.Symbol] += h.shares
	default:
		cost := order.MaxCost
		if order.Type == engine.Limit {
			if order.LimitPrice > math.MaxInt64/order.Quantity {
				return fmt.Errorf("%w: order cost overflows", ErrInvalidAmount)
			}
			cost = order.Quantity * order.LimitPrice
		}
		if avail := a.cash - a.cashHeld; avail < cost {
			return fmt.Errorf("%w: need %d, have %d available", ErrInsufficientFunds, cost, avail)
		}
		h.cash = cost
		a.cashHeld += h.cash
	}
	l.holds[order.ID] = h
	return nil
}

// Release removes an order's hold and frees whatever it still reserves.
func (l *Ledger) Release(orderID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.holds[orderID]; !ok {
		return fmt.Errorf("%w: %q", ErrUnknownHold, orderID)
	}
	l.release(orderID)
	return nil
}

// ApplyTick settles a tick's executions and releases the holds of expired
// orders. Ticks must be applied exactly once each, in order. A tick is applied
// atomically: if any part of it is rejected, nothing changes.
func (l *Ledger) ApplyTick(tr engine.TickResult) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case tr.Tick <= l.lastTick:
		return fmt.Errorf("%w: tick %d, last applied %d", ErrTickAlreadyApplied, tr.Tick, l.lastTick)
	case tr.Tick != l.lastTick+1:
		return fmt.Errorf("%w: tick %d, expected %d", ErrTickOutOfOrder, tr.Tick, l.lastTick+1)
	}
	if err := l.checkTick(tr); err != nil {
		return err
	}

	for _, book := range tr.Books {
		for _, e := range book.Executions {
			cost := e.Price * e.Quantity

			bh := l.holds[e.BuyOrderID]
			buyer := l.accounts[bh.order.AccountID]
			debit := cost
			if bh.order.Type == engine.Limit {
				debit = e.Quantity * bh.order.LimitPrice
			}
			bh.cash -= debit
			bh.remaining -= e.Quantity
			buyer.cashHeld -= debit
			buyer.cash -= cost
			buyer.shares[e.Symbol] += e.Quantity

			sh := l.holds[e.SellOrderID]
			seller := l.accounts[sh.order.AccountID]
			sh.shares -= e.Quantity
			sh.remaining -= e.Quantity
			seller.sharesHeld[e.Symbol] -= e.Quantity
			seller.shares[e.Symbol] -= e.Quantity
			seller.cash += cost

			for _, h := range []*hold{bh, sh} {
				if h.remaining == 0 {
					l.release(h.order.ID)
				}
			}
		}
		for _, id := range book.ExpiredOrderIDs {
			l.release(id)
		}
	}
	l.lastTick = tr.Tick
	return nil
}

// checkTick verifies, without changing state, that every execution and expiry
// in tr matches a hold that can fund it.
func (l *Ledger) checkTick(tr engine.TickResult) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: tick %d: %s", ErrInconsistentTick, tr.Tick, fmt.Sprintf(format, args...))
	}
	filled := map[string]int64{}
	spent := map[string]int64{}
	expired := map[string]bool{}
	match := func(id, account, symbol string, dir engine.Direction) (*hold, error) {
		h, ok := l.holds[id]
		switch {
		case !ok:
			return nil, bad("no hold for order %q", id)
		case expired[id]:
			return nil, bad("order %q executed after expiring", id)
		case h.order.AccountID != account || h.order.Symbol != symbol || h.order.Direction != dir:
			return nil, bad("execution does not match order %q", id)
		}
		return h, nil
	}

	for _, book := range tr.Books {
		for _, e := range book.Executions {
			if e.Quantity <= 0 || e.Price <= 0 || e.Price > math.MaxInt64/e.Quantity {
				return bad("invalid execution %+v", e)
			}
			bh, err := match(e.BuyOrderID, e.BuyAccountID, e.Symbol, engine.Buy)
			if err != nil {
				return err
			}
			sh, err := match(e.SellOrderID, e.SellAccountID, e.Symbol, engine.Sell)
			if err != nil {
				return err
			}
			if bh.order.Type == engine.Limit && e.Price > bh.order.LimitPrice {
				return bad("order %q bought above its limit", bh.order.ID)
			}
			if sh.order.Type == engine.Limit && e.Price < sh.order.LimitPrice {
				return bad("order %q sold below its limit", sh.order.ID)
			}
			filled[bh.order.ID] += e.Quantity
			filled[sh.order.ID] += e.Quantity
			spent[bh.order.ID] += e.Price * e.Quantity
			for _, h := range []*hold{bh, sh} {
				if filled[h.order.ID] > h.remaining {
					return bad("order %q overfilled", h.order.ID)
				}
			}
			if bh.order.Type == engine.Market && spent[bh.order.ID] > bh.cash {
				return bad("order %q exceeded max_cost", bh.order.ID)
			}
		}
		for _, id := range book.ExpiredOrderIDs {
			h, ok := l.holds[id]
			switch {
			case !ok:
				return bad("no hold for expired order %q", id)
			case expired[id]:
				return bad("order %q expired twice", id)
			case filled[id] == h.remaining:
				return bad("fully filled order %q expired", id)
			}
			expired[id] = true
		}
	}
	return nil
}

// release frees the remainder of a hold. The hold must exist.
func (l *Ledger) release(orderID string) {
	h := l.holds[orderID]
	a := l.accounts[h.order.AccountID]
	a.cashHeld -= h.cash
	a.sharesHeld[h.order.Symbol] -= h.shares
	if a.sharesHeld[h.order.Symbol] == 0 {
		delete(a.sharesHeld, h.order.Symbol)
	}
	if a.shares[h.order.Symbol] == 0 {
		delete(a.shares, h.order.Symbol)
	}
	delete(l.holds, orderID)
}

// Account returns a snapshot of an account.
func (l *Ledger) Account(id string) (Account, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, err := l.account(id)
	if err != nil {
		return Account{}, err
	}
	out := Account{ID: id, Cash: a.cash, CashHeld: a.cashHeld, Holdings: []Holding{}}
	for _, sym := range slices.Sorted(maps.Keys(a.shares)) {
		if a.shares[sym] > 0 {
			out.Holdings = append(out.Holdings, Holding{Symbol: sym, Quantity: a.shares[sym], Held: a.sharesHeld[sym]})
		}
	}
	return out, nil
}

// LastTick returns the most recent tick applied.
func (l *Ledger) LastTick() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastTick
}

func (l *Ledger) account(id string) (*account, error) {
	a, ok := l.accounts[id]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownAccount, id)
	}
	return a, nil
}
