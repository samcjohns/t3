package ledger

import (
	"context"
	"fmt"
	"maps"
	"math"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/samcjohns/t3/pkg/engine"
)

// The lending pool lets traders buy with leverage. Lenders put cash into the
// pool for pool shares, priced at the pool's value (its cash plus everything
// owed to it) divided by the shares outstanding. Interest raises what is
// owed, so the share price rises and every lender gains in proportion.
//
// A leveraged buy borrows cost × (ratio-1)/ratio from the pool as it fills.
// Each ratio has its own borrow index, which grows every tick by that ratio's
// rate. A loan is stored scaled by the index it was taken at, so what it owes
// is scaled × current index, and interest needs no per-loan work. Debt
// belongs to the account, not the purchase: proceeds of every sale repay it,
// most expensive ratio first.
//
// After each tick, an account whose equity has fallen below the maintenance
// margin is liquidated: it can no longer trade, its open orders are
// cancelled and its shares are sold at market. Once everything is sold its
// cash repays what it can. Whatever is still owed is written off, which
// lowers the pool's value and so every lender's stake, and the account is
// closed as bankrupt.

// Status is an account's standing with the lending pool.
type Status string

const (
	StatusActive Status = "active"
	// StatusLiquidating accounts cannot trade while the ledger sells their
	// holdings to repay their loans.
	StatusLiquidating Status = "liquidating"
	// StatusBankrupt accounts owed more than liquidation raised. They are
	// closed for good.
	StatusBankrupt Status = "bankrupt"
)

// Leverage configures the lending pool. Zero values select defaults.
type Leverage struct {
	// DailyRate is the base borrowing rate, in basis points per day.
	// Default 500 (5%).
	DailyRate int64
	// Premiums maps each allowed leverage ratio to the basis points per day
	// it adds to DailyRate. Default 2x +0, 3x +100, 5x +200.
	Premiums map[int64]int64
	// MaintenanceMargin is the equity, in basis points of the value of its
	// holdings, below which a borrowing account is liquidated. Default 1500.
	MaintenanceMargin int64
	// MaxTradeLoan caps what one order may borrow, and MaxTraderLoan what one
	// account may owe, in basis points of the pool's value when borrowing.
	// Defaults 500 (5%) and 5000 (50%), so no single trade or trader can put
	// much of the lenders' money at risk.
	MaxTradeLoan  int64
	MaxTraderLoan int64
	// TicksPerDay converts daily rates into per-tick interest. Default: a
	// day of engine.DefaultHeartbeat ticks.
	TicksPerDay int64
}

func (c *Leverage) setDefaults() {
	if c.DailyRate <= 0 {
		c.DailyRate = 500
	}
	if len(c.Premiums) == 0 {
		c.Premiums = map[int64]int64{2: 0, 3: 100, 5: 200}
	}
	if c.MaintenanceMargin <= 0 {
		c.MaintenanceMargin = 1500
	}
	if c.MaxTradeLoan <= 0 {
		c.MaxTradeLoan = 500
	}
	if c.MaxTraderLoan <= 0 {
		c.MaxTraderLoan = 5000
	}
	if c.TicksPerDay <= 0 {
		c.TicksPerDay = int64(24 * time.Hour / engine.DefaultHeartbeat)
	}
}

// kinkUtilization is the share of the pool lent out, in basis points, above
// which the base rate climbs, reaching three times DailyRate when the pool is
// fully lent. Dearer loans draw in lenders and put off borrowers.
const kinkUtilization = 8000

// rate is the borrowing rate at ratio, in basis points per day.
func (c Leverage) rate(ratio, utilization int64) int64 {
	base := c.DailyRate
	if utilization > kinkUtilization {
		base += base * 2 * (utilization - kinkUtilization) / (10_000 - kinkUtilization)
	}
	return base + c.Premiums[ratio]
}

// liquidationPrefix starts the IDs of the orders that liquidation places.
const liquidationPrefix = "liq-"

// initialSharesPerCent is how many pool shares a cent buys in an empty pool.
const initialSharesPerCent = 1000

var (
	ray = new(big.Int).Exp(big.NewInt(10), big.NewInt(27), nil)
	// debtScale converts cents to scaled debt at index ray. It is ray² so
	// that a cent stays representable as an index grows.
	debtScale = new(big.Int).Mul(ray, ray)
)

// mulDiv is a×b÷c, rounded down, or up when up is set. Arguments must be
// non-negative.
func mulDiv(a, b, c *big.Int, up bool) *big.Int {
	q, r := new(big.Int).QuoRem(new(big.Int).Mul(a, b), c, new(big.Int))
	if up && r.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	return q
}

// scale is a×b÷c for amounts whose result fits in an int64.
func scale(a, b, c int64, up bool) int64 {
	return mulDiv(big.NewInt(a), big.NewInt(b), big.NewInt(c), up).Int64()
}

// toScaled converts cents to scaled debt at index.
func toScaled(cents int64, index *big.Int, up bool) *big.Int {
	return mulDiv(big.NewInt(cents), debtScale, index, up)
}

// toCents converts scaled debt to cents at index.
func toCents(scaled, index *big.Int, up bool) int64 {
	return mulDiv(scaled, index, debtScale, up).Int64()
}

// pool is the lending pool. Its big.Int values are never modified in place,
// so clones can share them.
type pool struct {
	cash, reserved, shares int64
	// index and scaled are keyed by leverage ratio. A ratio without an index
	// has never accrued interest: its index is ray.
	index, scaled map[int64]*big.Int
}

func newPool(st PoolState) *pool {
	p := &pool{cash: st.Cash, reserved: st.Reserved, shares: st.Shares, index: maps.Clone(st.Index), scaled: maps.Clone(st.Scaled)}
	if p.index == nil {
		p.index = map[int64]*big.Int{}
	}
	if p.scaled == nil {
		p.scaled = map[int64]*big.Int{}
	}
	return p
}

func (p *pool) clone() *pool {
	c := *p
	c.index, c.scaled = maps.Clone(p.index), maps.Clone(p.scaled)
	return &c
}

func (p *pool) state() PoolState {
	return PoolState{Cash: p.cash, Reserved: p.reserved, Shares: p.shares, Index: maps.Clone(p.index), Scaled: maps.Clone(p.scaled)}
}

func (p *pool) indexOf(ratio int64) *big.Int {
	if i := p.index[ratio]; i != nil {
		return i
	}
	return ray
}

// lentAt is what borrowers at ratio owe, rounded down.
func (p *pool) lentAt(ratio int64) int64 {
	if s := p.scaled[ratio]; s != nil {
		return toCents(s, p.indexOf(ratio), false)
	}
	return 0
}

// lent is what all borrowers owe, rounded down.
func (p *pool) lent() int64 {
	var n int64
	for r := range p.scaled {
		n += p.lentAt(r)
	}
	return n
}

func (p *pool) value() int64 { return p.cash + p.lent() }

// available is the cash that can be borrowed or withdrawn.
func (p *pool) available() int64 { return p.cash - p.reserved }

// utilization is the share of the pool lent out, in basis points.
func (p *pool) utilization() int64 {
	v := p.value()
	if v <= 0 {
		return 0
	}
	return scale(p.lent(), 10_000, v, false)
}

// stake is what shares of the pool are worth, rounded down.
func (p *pool) stake(shares int64) int64 {
	if shares <= 0 || p.shares <= 0 {
		return 0
	}
	return scale(shares, p.value(), p.shares, false)
}

// owed is what loans owe, rounded up.
func (p *pool) owed(loans map[int64]*big.Int) int64 {
	var n int64
	for r, s := range loans {
		n += toCents(s, p.indexOf(r), true)
	}
	return n
}

func addScaled(m map[int64]*big.Int, ratio int64, delta *big.Int) {
	v := new(big.Int).Add(delta, orZero(m[ratio]))
	if v.Sign() == 0 {
		delete(m, ratio)
	} else {
		m[ratio] = v
	}
}

func orZero(v *big.Int) *big.Int {
	if v == nil {
		return new(big.Int)
	}
	return v
}

// poolView is the pool as this transaction sees it, for reading only.
func (t *txn) poolView() *pool {
	if t.pool != nil {
		return t.pool
	}
	return t.l.pool
}

// stagedPool returns the transaction's copy of the pool, for changing.
func (t *txn) stagedPool() *pool {
	if t.pool == nil {
		t.pool = t.l.pool.clone()
	}
	return t.pool
}

// borrow lends amount of the pool's cash to a at ratio.
func (t *txn) borrow(a *account, ratio, amount int64) {
	p := t.stagedPool()
	s := toScaled(amount, p.indexOf(ratio), true)
	if a.loans == nil {
		a.loans = map[int64]*big.Int{}
	}
	addScaled(a.loans, ratio, s)
	addScaled(p.scaled, ratio, s)
	p.cash -= amount
	a.cash += amount
}

// repay pays up to amount of a's cash towards its loans, most expensive
// first, and returns what it paid.
func (t *txn) repay(a *account, amount int64) int64 {
	if amount <= 0 || len(a.loans) == 0 {
		return 0
	}
	p := t.stagedPool()
	var paid int64
	for _, r := range slices.Backward(slices.Sorted(maps.Keys(a.loans))) {
		left := amount - paid
		if left == 0 {
			break
		}
		s, idx := a.loans[r], p.indexOf(r)
		part, cents := s, toCents(s, idx, true)
		if left < cents {
			// Rounding down leaves the rest of the debt at least as large.
			part, cents = toScaled(left, idx, false), left
		}
		neg := new(big.Int).Neg(part)
		addScaled(a.loans, r, neg)
		addScaled(p.scaled, r, neg)
		paid += cents
	}
	if len(a.loans) == 0 {
		a.loans = nil
	}
	p.cash += paid
	a.cash -= paid
	return paid
}

// writeOff cancels all of a's loans and returns what they owed, which the
// pool's lenders lose.
func (t *txn) writeOff(a *account) int64 {
	p := t.stagedPool()
	owed := p.owed(a.loans)
	for r, s := range a.loans {
		addScaled(p.scaled, r, new(big.Int).Neg(s))
	}
	a.loans = nil
	// A pool that has lost everything starts again: its shares are worthless.
	if p.shares > 0 && p.value() == 0 {
		for id, acc := range t.l.accounts {
			if acc.poolShares > 0 {
				staged, _ := t.account(id)
				staged.poolShares = 0
			}
		}
		p.shares = 0
	}
	return owed
}

// peek returns the account as this transaction sees it, for reading only.
func (t *txn) peek(id string) *account {
	if a, ok := t.accounts[id]; ok {
		return a
	}
	return t.l.accounts[id]
}

func (t *txn) mark(symbol string) int64 {
	if m, ok := t.marks[symbol]; ok {
		return m
	}
	return t.l.marks[symbol]
}

// valuation is the value of a's holdings at last prices, and its equity:
// everything it has less everything it owes.
func (t *txn) valuation(a *account) (holdings, equity int64) {
	for sym, q := range a.shares {
		holdings += q * t.mark(sym)
	}
	p := t.poolView()
	return holdings, a.cash + holdings + p.stake(a.poolShares) - p.owed(a.loans)
}

// pendingLoans is the pool cash set aside for an account's open orders.
func (t *txn) pendingLoans(accountID string) int64 {
	var n int64
	for id, h := range t.l.holds {
		if s, ok := t.holds[id]; ok {
			h = s
		}
		if h != nil && h.order.AccountID == accountID {
			n += h.loan
		}
	}
	return n
}

func (a *account) canTrade() error {
	switch a.status {
	case StatusLiquidating:
		return ErrAccountLiquidating
	case StatusBankrupt:
		return ErrAccountClosed
	}
	if a.blocked {
		return ErrAccountBlocked
	}
	return nil
}

// accrue charges one tick of interest on every loan.
func (t *txn) accrue() {
	if len(t.poolView().scaled) == 0 {
		return
	}
	p := t.stagedPool()
	util := p.utilization()
	perDay := big.NewInt(10_000 * t.l.lev.TicksPerDay)
	for r := range p.scaled {
		growth := new(big.Int).Add(ray, mulDiv(ray, big.NewInt(t.l.lev.rate(r, util)), perDay, false))
		p.index[r] = mulDiv(p.indexOf(r), growth, ray, true)
	}
}

// enforceMargins starts liquidating every borrower whose equity is below the
// maintenance margin, and closes out each liquidation that has sold
// everything.
func (t *txn) enforceMargins(tick uint64) {
	l := t.l
	busy := map[string]bool{}
	for id, h := range l.holds {
		if s, ok := t.holds[id]; ok {
			h = s
		}
		if h != nil {
			busy[h.order.AccountID] = true
		}
	}
	for _, id := range slices.Sorted(maps.Keys(l.accounts)) {
		a := t.peek(id)
		switch {
		case a.status == StatusActive && len(a.loans) > 0:
			holdings, equity := t.valuation(a)
			if equity*10_000 >= l.lev.MaintenanceMargin*holdings {
				continue
			}
			a, _ = t.account(id)
			a.status = StatusLiquidating
		case a.status != StatusLiquidating:
			continue
		}
		if !busy[id] && !slices.ContainsFunc(slices.Collect(maps.Values(a.shares)), func(q int64) bool { return q > 0 }) {
			t.closeOut(id, tick)
		}
	}
}

// closeOut ends a liquidation that has sold everything. Cash repays what it
// can; any debt left is written off and the account is closed.
func (t *txn) closeOut(id string, tick uint64) {
	a, _ := t.account(id)
	if paid := t.repay(a, a.cash-a.cashHeld); paid > 0 {
		t.entry(Entry{AccountID: id, Kind: EntryRepay, Tick: tick, CashDelta: -paid})
	}
	if len(a.loans) == 0 {
		a.status = StatusActive
		return
	}
	a.status = StatusBankrupt
	t.bankruptcies = append(t.bankruptcies, Bankruptcy{AccountID: id, Tick: tick, Time: t.l.clock(), WrittenOff: t.writeOff(a)})
}

// Bankruptcy records an account closed with debt it could not repay.
type Bankruptcy struct {
	AccountID string    `json:"account_id"`
	Tick      uint64    `json:"tick"`
	Time      time.Time `json:"time"`
	// WrittenOff is the debt left unpaid, which the pool's lenders lost.
	WrittenOff int64 `json:"written_off"`
}

// ReserveLeveraged is Reserve for a buy that borrows from the pool at ratio,
// one of the configured leverage ratios: the account funds cost ÷ ratio and
// the pool sets aside the rest. A ratio of 1 borrows nothing.
//
// Borrowing is refused to an account with a stake in the pool, when the
// order would borrow more than MaxTradeLoan of the pool or the account would
// owe more than MaxTraderLoan of it, and when what the account would owe
// exceeds ratio-1 times its equity.
func (l *Ledger) ReserveLeveraged(ctx context.Context, order engine.Order, ratio int64) error {
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
	if err := a.canTrade(); err != nil {
		return err
	}
	if _, ok := l.holds[order.ID]; ok {
		return fmt.Errorf("%w: %q", ErrDuplicateHold, order.ID)
	}

	h := &hold{order: order, remaining: order.Quantity, createdAt: l.clock()}
	switch {
	case ratio > 1 && order.Direction != engine.Buy:
		return fmt.Errorf("%w: only buys can use leverage", ErrLeverage)
	case ratio > 1 && !l.allowedRatio(ratio):
		return fmt.Errorf("%w: ratio must be one of %s", ErrLeverage, l.ratioList())
	case ratio > 1 && a.poolShares > 0:
		return fmt.Errorf("%w: withdraw your stake in the lending pool before borrowing", ErrLeverage)
	case ratio < 1:
		return fmt.Errorf("%w: ratio must be at least 1", ErrLeverage)
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
		if ratio > 1 {
			h.leverage = ratio
			h.loan = scale(cost, ratio-1, ratio, false)
		}
		h.cash = cost - h.loan
		if avail := a.cash - a.cashHeld; avail < h.cash {
			return fmt.Errorf("%w: need %d, have %d available", ErrInsufficientFunds, h.cash, avail)
		}
		if h.loan > 0 {
			p := t.stagedPool()
			if avail := p.available(); avail < h.loan {
				return fmt.Errorf("%w: need to borrow %d, the pool has %d available", ErrPoolLiquidity, h.loan, avail)
			}
			tradeMax, traderMax := l.loanLimits(p)
			owed := p.owed(a.loans) + t.pendingLoans(order.AccountID) + h.loan
			switch {
			case h.loan > tradeMax:
				return fmt.Errorf("%w: this order would borrow %d, and one order may borrow at most %d", ErrLoanLimit, h.loan, tradeMax)
			case owed > traderMax:
				return fmt.Errorf("%w: you would owe %d, and one trader may owe at most %d", ErrLoanLimit, owed, traderMax)
			}
			if _, equity := t.valuation(a); owed > (ratio-1)*equity {
				return fmt.Errorf("%w: you would owe %d, more than %dx your equity of %d", ErrLeverage, owed, ratio-1, equity)
			}
			p.reserved += h.loan
		}
		a.cashHeld += h.cash
	}
	t.holds[order.ID] = h
	return t.commit(ctx)
}

// loanLimits are the most one order may borrow, and one account may owe,
// given the pool's current value.
func (l *Ledger) loanLimits(p *pool) (trade, trader int64) {
	v := p.value()
	return scale(v, l.lev.MaxTradeLoan, 10_000, false), scale(v, l.lev.MaxTraderLoan, 10_000, false)
}

func (l *Ledger) allowedRatio(ratio int64) bool {
	_, ok := l.lev.Premiums[ratio]
	return ok
}

func (l *Ledger) ratioList() string {
	var s []string
	for _, r := range slices.Sorted(maps.Keys(l.lev.Premiums)) {
		s = append(s, fmt.Sprintf("%dx", r))
	}
	return strings.Join(s, ", ")
}

// split divides what a fill releases from a buy's hold between the buyer's
// cash and the pool cash set aside for it.
func (h *hold) split(debit int64) (own, loan int64) {
	if h.leverage > 1 {
		loan = min(h.loan, scale(debit, h.leverage-1, h.leverage, false))
	}
	own = debit - loan
	if own > h.cash {
		own = h.cash
		loan = debit - own
	}
	return own, loan
}

// Lend moves cash from an account into the lending pool, in exchange for
// pool shares at the current share price. An account that owes the pool
// cannot lend to it.
func (l *Ledger) Lend(ctx context.Context, accountID string, amount int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.begin()
	a, err := t.account(accountID)
	if err != nil {
		return err
	}
	if err := a.canTrade(); err != nil {
		return err
	}
	if amount <= 0 || amount > math.MaxInt64/initialSharesPerCent {
		return fmt.Errorf("%w: lend %d", ErrInvalidAmount, amount)
	}
	if len(a.loans) > 0 || t.pendingLoans(accountID) > 0 {
		return fmt.Errorf("%w: repay what you owe the pool before lending to it", ErrLeverage)
	}
	if avail := a.cash - a.cashHeld; amount > avail {
		return fmt.Errorf("%w: need %d, have %d available", ErrInsufficientFunds, amount, avail)
	}
	p := t.stagedPool()
	minted := amount * initialSharesPerCent
	if p.shares > 0 {
		minted = scale(amount, p.shares, p.value(), false)
	}
	if minted <= 0 {
		return fmt.Errorf("%w: lend %d is too small", ErrInvalidAmount, amount)
	}
	a.cash -= amount
	a.poolShares += minted
	p.cash += amount
	p.shares += minted
	t.entry(Entry{AccountID: accountID, Kind: EntryLend, CashDelta: -amount})
	return t.commit(ctx)
}

// Redeem pays amount out of an account's stake in the pool, or its whole
// stake if amount is 0, and returns what it paid. Only cash the pool has not
// lent out or set aside can be withdrawn.
func (l *Ledger) Redeem(ctx context.Context, accountID string, amount int64) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.begin()
	a, err := t.account(accountID)
	if err != nil {
		return 0, err
	}
	p := t.stagedPool()
	stake := p.stake(a.poolShares)
	switch {
	case amount < 0:
		return 0, fmt.Errorf("%w: withdraw %d", ErrInvalidAmount, amount)
	case a.poolShares == 0:
		return 0, fmt.Errorf("%w: you have nothing in the lending pool", ErrInsufficientFunds)
	case amount > stake:
		return 0, fmt.Errorf("%w: your stake is worth %d", ErrInsufficientFunds, stake)
	}
	burn := a.poolShares
	if amount == 0 {
		amount = stake
	} else {
		burn = min(burn, scale(amount, p.shares, p.value(), true))
	}
	// A remainder worth less than a cent goes too.
	if p.stake(a.poolShares-burn) == 0 {
		burn = a.poolShares
	}
	if avail := p.available(); amount > avail {
		return 0, fmt.Errorf("%w: the pool has %d available to withdraw", ErrPoolLiquidity, avail)
	}
	a.poolShares -= burn
	p.shares -= burn
	p.cash -= amount
	a.cash += amount
	t.entry(Entry{AccountID: accountID, Kind: EntryRedeem, CashDelta: amount})
	return amount, t.commit(ctx)
}

// Repay pays up to amount of an account's available cash towards what it
// owes the pool, and returns what it paid.
func (l *Ledger) Repay(ctx context.Context, accountID string, amount int64) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.begin()
	a, err := t.account(accountID)
	if err != nil {
		return 0, err
	}
	switch {
	case amount <= 0:
		return 0, fmt.Errorf("%w: repay %d", ErrInvalidAmount, amount)
	case len(a.loans) == 0:
		return 0, fmt.Errorf("%w: you owe the pool nothing", ErrInvalidAmount)
	}
	if avail := a.cash - a.cashHeld; amount > avail {
		amount = avail
	}
	paid := t.repay(a, amount)
	if paid == 0 {
		return 0, fmt.Errorf("%w: you have no cash available", ErrInsufficientFunds)
	}
	t.entry(Entry{AccountID: accountID, Kind: EntryRepay, CashDelta: -paid})
	return paid, t.commit(ctx)
}

// PoolSummary describes the lending pool. Rates are in basis points per day.
type PoolSummary struct {
	// Value is everything the pool has: Cash plus Lent.
	Value int64 `json:"value"`
	// Cash is not lent out. Available is the part of it not set aside for
	// open orders, which can be borrowed or withdrawn.
	Cash      int64 `json:"cash"`
	Available int64 `json:"available"`
	// Lent is what borrowers owe, interest included.
	Lent        int64 `json:"lent"`
	Utilization int64 `json:"utilization_bps"`
	// LenderRate is what lenders earn now: the interest borrowers pay,
	// spread over the whole pool.
	LenderRate int64       `json:"lender_rate_bps"`
	BaseRate   int64       `json:"base_rate_bps"`
	Rates      []RatioRate `json:"rates"`
	// MaintenanceMargin is the equity, as a share of holdings, below which
	// a borrower is liquidated.
	MaintenanceMargin int64 `json:"maintenance_margin_bps"`
	// MaxTradeLoan is the most one order may borrow now, and MaxTraderLoan
	// the most one trader may owe: the ...Bps shares of Value.
	MaxTradeLoan     int64 `json:"max_trade_loan"`
	MaxTradeLoanBps  int64 `json:"max_trade_loan_bps"`
	MaxTraderLoan    int64 `json:"max_trader_loan"`
	MaxTraderLoanBps int64 `json:"max_trader_loan_bps"`
	Lenders          int   `json:"lenders"`
	Borrowers        int   `json:"borrowers"`
}

// RatioRate is what borrowing at one leverage ratio costs now.
type RatioRate struct {
	Ratio int64 `json:"ratio"`
	Rate  int64 `json:"rate_bps"`
}

// Pool returns a summary of the lending pool.
func (l *Ledger) Pool() PoolSummary {
	l.mu.Lock()
	defer l.mu.Unlock()
	p := l.pool
	util := p.utilization()
	s := PoolSummary{
		Value: p.value(), Cash: p.cash, Available: p.available(), Lent: p.lent(), Utilization: util,
		BaseRate: l.lev.rate(0, util), Rates: []RatioRate{}, MaintenanceMargin: l.lev.MaintenanceMargin,
		MaxTradeLoanBps: l.lev.MaxTradeLoan, MaxTraderLoanBps: l.lev.MaxTraderLoan,
	}
	s.MaxTradeLoan, s.MaxTraderLoan = l.loanLimits(p)
	var earning int64
	for _, r := range slices.Sorted(maps.Keys(l.lev.Premiums)) {
		rate := l.lev.rate(r, util)
		s.Rates = append(s.Rates, RatioRate{r, rate})
		earning += p.lentAt(r) * rate
	}
	if s.Value > 0 {
		s.LenderRate = earning / s.Value
	}
	for _, a := range l.accounts {
		if a.poolShares > 0 {
			s.Lenders++
		}
		if len(a.loans) > 0 {
			s.Borrowers++
		}
	}
	return s
}

// Bankruptcies returns every bankruptcy, newest first.
func (l *Ledger) Bankruptcies() []Bankruptcy {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := slices.Clone(l.bankruptcies)
	slices.Reverse(out)
	return out
}

// Liquidation is what the host must do in the engine for accounts being
// liquidated: cancel their open orders, then submit market sells of their
// available shares. Blocked accounts have their open orders cancelled too. The sells' holds are already placed; release any the
// engine rejects.
type Liquidation struct {
	Cancel []string
	Sell   []engine.Order
}

// SetBlocked blocks an account from placing orders and lending, or lifts
// the block. Liquidations cancels a blocked account's open orders.
func (l *Ledger) SetBlocked(ctx context.Context, accountID string, blocked bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.begin()
	a, err := t.account(accountID)
	if err != nil {
		return err
	}
	a.blocked = blocked
	return t.commit(ctx)
}

// Liquidate starts liquidating an account at an admin's request, as if it
// had fallen below the maintenance margin: its open orders are cancelled and
// its shares sold at market. Once everything is sold, its cash repays what it
// owes and it becomes active again, or bankrupt if it cannot repay.
func (l *Ledger) Liquidate(ctx context.Context, accountID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.begin()
	a, err := t.account(accountID)
	if err != nil {
		return err
	}
	switch a.status {
	case StatusBankrupt:
		return ErrAccountClosed
	case StatusLiquidating:
		return nil
	}
	a.status = StatusLiquidating
	return t.commit(ctx)
}

// Liquidations returns the next steps of every liquidation. Call it after
// each tick is applied, until it returns nothing to do: cancellations are
// not durable and sells may only partly fill, so steps repeat until the
// account has nothing left to sell.
func (l *Ledger) Liquidations(ctx context.Context) (Liquidation, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out Liquidation
	t := l.begin()
	for _, id := range slices.Sorted(maps.Keys(l.accounts)) {
		acc := l.accounts[id]
		if acc.status != StatusLiquidating && !acc.blocked {
			continue
		}
		for _, oid := range slices.Sorted(maps.Keys(l.holds)) {
			if l.holds[oid].order.AccountID == id && !strings.HasPrefix(oid, liquidationPrefix) {
				out.Cancel = append(out.Cancel, oid)
			}
		}
		if acc.status != StatusLiquidating {
			continue
		}
		a, _ := t.account(id)
		for _, sym := range slices.Sorted(maps.Keys(a.shares)) {
			avail := a.shares[sym] - a.sharesHeld[sym]
			o := engine.Order{
				ID: fmt.Sprintf("%s%d-%s-%s", liquidationPrefix, l.lastTick, id, sym), AccountID: id,
				Symbol: sym, Direction: engine.Sell, Type: engine.Market, Quantity: avail,
			}
			if _, dup := l.holds[o.ID]; avail <= 0 || dup {
				continue
			}
			t.holds[o.ID] = &hold{order: o, remaining: avail, shares: avail, createdAt: l.clock()}
			a.sharesHeld[sym] += avail
			out.Sell = append(out.Sell, o)
		}
	}
	if len(out.Sell) == 0 {
		return out, nil
	}
	return out, t.commit(ctx)
}
