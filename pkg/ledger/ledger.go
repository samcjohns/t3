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
//
// With a Store, every operation is persisted before it takes effect in
// memory, so a failed write changes nothing.
package ledger

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"sync"
	"time"

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

// TickSource supplies committed ticks for catching up.
type TickSource interface {
	TicksAfter(ctx context.Context, after uint64, limit int) ([]engine.TickResult, error)
}

// OrderChecker reports which orders the engine has ever accepted.
type OrderChecker interface {
	KnownOrders(ctx context.Context, ids []string) (map[string]bool, error)
}

type account struct {
	cash, cashHeld     int64
	shares, sharesHeld map[string]int64
}

func (a *account) clone() *account {
	return &account{a.cash, a.cashHeld, maps.Clone(a.shares), maps.Clone(a.sharesHeld)}
}

func (a *account) snapshot(id string) Account {
	out := Account{ID: id, Cash: a.cash, CashHeld: a.cashHeld, Holdings: []Holding{}}
	for _, sym := range slices.Sorted(maps.Keys(a.shares)) {
		if a.shares[sym] > 0 {
			out.Holdings = append(out.Holdings, Holding{Symbol: sym, Quantity: a.shares[sym], Held: a.sharesHeld[sym]})
		}
	}
	return out
}

// hold reserves the funding for one open order.
type hold struct {
	order engine.Order
	// remaining is the order's unfilled quantity.
	remaining int64
	// cash and shares are what is still held for the order.
	cash, shares int64
	createdAt    time.Time
}

func (h *hold) state() HoldState {
	return HoldState{Order: h.order, Remaining: h.remaining, Cash: h.cash, Shares: h.shares, CreatedAt: h.createdAt}
}

// Config configures a Ledger. Zero values select defaults.
type Config struct {
	// Store persists the ledger. Without one, state is in memory only.
	Store Store
	// Clock timestamps holds and entries. Defaults to time.Now.
	Clock func() time.Time
}

type refKey struct{ account, ref string }

// Ledger is safe for concurrent use.
type Ledger struct {
	store Store
	clock func() time.Time

	mu       sync.Mutex
	accounts map[string]*account
	holds    map[string]*hold
	refs     map[refKey]struct{}
	lastTick uint64
}

// New returns an empty in-memory ledger.
func New() *Ledger {
	l, _ := Open(context.Background(), Config{})
	return l
}

// Open returns a ledger restored from cfg.Store.
func Open(ctx context.Context, cfg Config) (*Ledger, error) {
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	l := &Ledger{
		store:    cfg.Store,
		clock:    cfg.Clock,
		accounts: make(map[string]*account),
		holds:    make(map[string]*hold),
		refs:     make(map[refKey]struct{}),
	}
	if l.store == nil {
		return l, nil
	}
	st, err := l.store.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading ledger: %w", err)
	}
	for _, a := range st.Accounts {
		acc := &account{cash: a.Cash, cashHeld: a.CashHeld, shares: map[string]int64{}, sharesHeld: map[string]int64{}}
		for _, h := range a.Holdings {
			acc.shares[h.Symbol] = h.Quantity
			if h.Held > 0 {
				acc.sharesHeld[h.Symbol] = h.Held
			}
		}
		l.accounts[a.ID] = acc
	}
	for _, h := range st.Holds {
		l.holds[h.Order.ID] = &hold{order: h.Order, remaining: h.Remaining, cash: h.Cash, shares: h.Shares, createdAt: h.CreatedAt}
	}
	for _, r := range st.References {
		l.refs[refKey{r.AccountID, r.Reference}] = struct{}{}
	}
	l.lastTick = st.LastTick
	return l, nil
}

// txn stages one operation on copies of the state it touches. Nothing is
// visible until commit persists it.
type txn struct {
	l        *Ledger
	accounts map[string]*account
	holds    map[string]*hold // nil marks a deleted hold
	entries  []Entry
	lastTick *uint64
}

// begin must be called with l.mu held.
func (l *Ledger) begin() *txn {
	return &txn{l: l, accounts: map[string]*account{}, holds: map[string]*hold{}}
}

func (t *txn) account(id string) (*account, error) {
	if a, ok := t.accounts[id]; ok {
		return a, nil
	}
	a, ok := t.l.accounts[id]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownAccount, id)
	}
	a = a.clone()
	t.accounts[id] = a
	return a, nil
}

// hold returns a staged copy of a hold, or nil if there is none.
func (t *txn) hold(id string) *hold {
	if h, ok := t.holds[id]; ok {
		return h
	}
	h, ok := t.l.holds[id]
	if !ok {
		return nil
	}
	c := *h
	t.holds[id] = &c
	return &c
}

func (t *txn) entry(e Entry) {
	e.CreatedAt = t.l.clock()
	t.entries = append(t.entries, e)
}

// release frees the remainder of a hold. The hold must exist.
func (t *txn) release(orderID string) {
	h := t.hold(orderID)
	a, _ := t.account(h.order.AccountID)
	sym := h.order.Symbol
	a.cashHeld -= h.cash
	a.sharesHeld[sym] -= h.shares
	if a.sharesHeld[sym] == 0 {
		delete(a.sharesHeld, sym)
	}
	if a.shares[sym] == 0 {
		delete(a.shares, sym)
	}
	t.holds[orderID] = nil
}

func (t *txn) commit(ctx context.Context) error {
	var ch Changes
	for _, id := range slices.Sorted(maps.Keys(t.accounts)) {
		ch.Accounts = append(ch.Accounts, t.accounts[id].snapshot(id))
	}
	for _, id := range slices.Sorted(maps.Keys(t.holds)) {
		if h := t.holds[id]; h == nil {
			ch.DeleteHolds = append(ch.DeleteHolds, id)
		} else {
			ch.PutHolds = append(ch.PutHolds, h.state())
		}
	}
	ch.Entries = t.entries
	ch.LastTick = t.lastTick

	if t.l.store != nil {
		if err := t.l.store.Apply(ctx, ch); err != nil {
			return fmt.Errorf("persisting ledger change: %w", err)
		}
	}

	l := t.l
	maps.Copy(l.accounts, t.accounts)
	for id, h := range t.holds {
		if h == nil {
			delete(l.holds, id)
		} else {
			l.holds[id] = h
		}
	}
	for _, e := range t.entries {
		if e.Reference != "" {
			l.refs[refKey{e.AccountID, e.Reference}] = struct{}{}
		}
	}
	if t.lastTick != nil {
		l.lastTick = *t.lastTick
	}
	return nil
}

func (l *Ledger) OpenAccount(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("%w: account id is required", ErrInvalidAmount)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.accounts[id]; ok {
		return fmt.Errorf("%w: %q", ErrAccountExists, id)
	}
	t := l.begin()
	t.accounts[id] = &account{shares: map[string]int64{}, sharesHeld: map[string]int64{}}
	return t.commit(ctx)
}

// Deposit credits cash to an account. A non-empty reference makes the
// deposit idempotent: a repeat with the same reference does nothing.
func (l *Ledger) Deposit(ctx context.Context, accountID string, amount int64, reference string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.begin()
	a, err := t.account(accountID)
	if err != nil {
		return err
	}
	if _, done := l.refs[refKey{accountID, reference}]; reference != "" && done {
		return nil
	}
	if amount <= 0 || a.cash > math.MaxInt64-amount {
		return fmt.Errorf("%w: deposit %d", ErrInvalidAmount, amount)
	}
	a.cash += amount
	t.entry(Entry{AccountID: accountID, Kind: EntryDeposit, Reference: reference, CashDelta: amount})
	return t.commit(ctx)
}

// DepositShares credits shares to an account, e.g. to seed market makers. A
// non-empty reference makes it idempotent, as for Deposit.
func (l *Ledger) DepositShares(ctx context.Context, accountID, symbol string, quantity int64, reference string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.begin()
	a, err := t.account(accountID)
	if err != nil {
		return err
	}
	if _, done := l.refs[refKey{accountID, reference}]; reference != "" && done {
		return nil
	}
	if symbol == "" || quantity <= 0 || a.shares[symbol] > math.MaxInt64-quantity {
		return fmt.Errorf("%w: deposit %d %q", ErrInvalidAmount, quantity, symbol)
	}
	a.shares[symbol] += quantity
	t.entry(Entry{AccountID: accountID, Kind: EntryShareDeposit, Reference: reference, Symbol: symbol, ShareDelta: quantity})
	return t.commit(ctx)
}

// Reserve places a hold funding the order. It must succeed before the order
// is submitted to the engine; if the engine then rejects the order, call
// Release.
func (l *Ledger) Reserve(ctx context.Context, order engine.Order) error {
	if err := order.Validate(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.begin()
	a, err := t.account(order.AccountID)
	if err != nil {
		return err
	}
	if _, ok := l.holds[order.ID]; ok {
		return fmt.Errorf("%w: %q", ErrDuplicateHold, order.ID)
	}

	h := &hold{order: order, remaining: order.Quantity, createdAt: l.clock()}
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
	t.holds[order.ID] = h
	return t.commit(ctx)
}

// Release removes an order's hold and frees whatever it still reserves.
func (l *Ledger) Release(ctx context.Context, orderID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.holds[orderID]; !ok {
		return fmt.Errorf("%w: %q", ErrUnknownHold, orderID)
	}
	t := l.begin()
	t.release(orderID)
	return t.commit(ctx)
}

// ApplyTick settles a tick's executions and releases the holds of expired
// orders. Ticks must be applied exactly once each, in order. A tick is applied
// atomically: if any part of it is rejected or fails to persist, nothing
// changes.
func (l *Ledger) ApplyTick(ctx context.Context, tr engine.TickResult) error {
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

	t := l.begin()
	for _, book := range tr.Books {
		for _, e := range book.Executions {
			cost := e.Price * e.Quantity

			bh := t.hold(e.BuyOrderID)
			buyer, _ := t.account(bh.order.AccountID)
			debit := cost
			if bh.order.Type == engine.Limit {
				debit = e.Quantity * bh.order.LimitPrice
			}
			bh.cash -= debit
			bh.remaining -= e.Quantity
			buyer.cashHeld -= debit
			buyer.cash -= cost
			buyer.shares[e.Symbol] += e.Quantity
			t.entry(Entry{AccountID: bh.order.AccountID, Kind: EntryBuy, Tick: tr.Tick, OrderID: bh.order.ID,
				CashDelta: -cost, Symbol: e.Symbol, ShareDelta: e.Quantity})

			sh := t.hold(e.SellOrderID)
			seller, _ := t.account(sh.order.AccountID)
			sh.shares -= e.Quantity
			sh.remaining -= e.Quantity
			seller.sharesHeld[e.Symbol] -= e.Quantity
			seller.shares[e.Symbol] -= e.Quantity
			seller.cash += cost
			t.entry(Entry{AccountID: sh.order.AccountID, Kind: EntrySell, Tick: tr.Tick, OrderID: sh.order.ID,
				CashDelta: cost, Symbol: e.Symbol, ShareDelta: -e.Quantity})

			for _, h := range []*hold{bh, sh} {
				if h.remaining == 0 {
					t.release(h.order.ID)
				}
			}
		}
		for _, id := range book.ExpiredOrderIDs {
			t.release(id)
		}
	}
	t.lastTick = &tr.Tick
	return t.commit(ctx)
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

// CatchUp applies every tick src has committed after LastTick.
func (l *Ledger) CatchUp(ctx context.Context, src TickSource) error {
	for {
		ticks, err := src.TicksAfter(ctx, l.LastTick(), 500)
		if err != nil {
			return fmt.Errorf("fetching ticks: %w", err)
		}
		if len(ticks) == 0 {
			return nil
		}
		for _, tr := range ticks {
			if err := l.ApplyTick(ctx, tr); err != nil && !errors.Is(err, ErrTickAlreadyApplied) {
				return err
			}
		}
	}
}

// ReconcileHolds releases holds older than grace whose order the engine has
// never accepted: the gateway reserved them and failed before submitting. The
// grace period must comfortably exceed the time a submission can take.
func (l *Ledger) ReconcileHolds(ctx context.Context, checker OrderChecker, grace time.Duration) ([]string, error) {
	l.mu.Lock()
	cutoff := l.clock().Add(-grace)
	var candidates []string
	for id, h := range l.holds {
		if h.createdAt.Before(cutoff) {
			candidates = append(candidates, id)
		}
	}
	l.mu.Unlock()
	if len(candidates) == 0 {
		return nil, nil
	}
	slices.Sort(candidates)

	known, err := checker.KnownOrders(ctx, candidates)
	if err != nil {
		return nil, fmt.Errorf("checking orders: %w", err)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.begin()
	var released []string
	for _, id := range candidates {
		if _, ok := l.holds[id]; ok && !known[id] {
			t.release(id)
			released = append(released, id)
		}
	}
	if len(released) == 0 {
		return nil, nil
	}
	if err := t.commit(ctx); err != nil {
		return nil, err
	}
	return released, nil
}

// Account returns a snapshot of an account.
func (l *Ledger) Account(id string) (Account, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.accounts[id]
	if !ok {
		return Account{}, fmt.Errorf("%w: %q", ErrUnknownAccount, id)
	}
	return a.snapshot(id), nil
}

// LastTick returns the most recent tick applied.
func (l *Ledger) LastTick() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastTick
}
