package gateway

import (
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/samcjohns/t3/pkg/engine"
	"github.com/samcjohns/t3/pkg/ledger"
)

// openOrder is one of a trader's orders that has not yet filled or expired.
type openOrder struct {
	ID          string             `json:"id"`
	Symbol      string             `json:"symbol"`
	Direction   engine.Direction   `json:"direction"`
	Type        engine.OrderType   `json:"type"`
	TimeInForce engine.TimeInForce `json:"time_in_force,omitempty"`
	// Quantity is what was ordered and Remaining what is still unfilled.
	Quantity   int64 `json:"quantity"`
	Remaining  int64 `json:"remaining"`
	LimitPrice int64 `json:"limit_price"`
	MaxCost    int64 `json:"max_cost"`
	Leverage   int64 `json:"leverage"`
	// CashHeld and SharesHeld are what the order still sets aside, which a
	// cancellation frees.
	CashHeld   int64     `json:"cash_held"`
	SharesHeld int64     `json:"shares_held"`
	CreatedAt  time.Time `json:"created_at"`
	// Status is "open", or "cancelling" once the trader has asked to cancel
	// it and the next auction has yet to remove it.
	Status string `json:"status"`
}

func newOpenOrder(h ledger.HoldState, cancelling bool) openOrder {
	o := openOrder{
		ID: h.Order.ID, Symbol: h.Order.Symbol, Direction: h.Order.Direction, Type: h.Order.Type,
		TimeInForce: h.Order.TimeInForce, Quantity: h.Order.Quantity, Remaining: h.Remaining,
		LimitPrice: h.Order.LimitPrice, MaxCost: h.Order.MaxCost, Leverage: max(h.Leverage, 1),
		CashHeld: h.Cash, SharesHeld: h.Shares, CreatedAt: h.CreatedAt, Status: "open",
	}
	if o.Type == engine.Limit && o.TimeInForce == "" {
		o.TimeInForce = engine.GTC
	}
	if cancelling {
		o.Status = "cancelling"
	}
	return o
}

// cancellations remembers which orders traders have asked to cancel, so
// listings show them as cancelling until the auction removes them. Like the
// engine's own queue of cancellations, it is lost on restart.
type cancellations struct {
	mu sync.Mutex
	// ids maps an account to the IDs of its orders being cancelled.
	ids map[string]map[string]struct{}
}

// sync forgets the account's cancellations of orders that are no longer
// open, adds id if it is not empty, and returns which open orders are being
// cancelled.
func (c *cancellations) sync(accountID string, open []ledger.HoldState, id string) map[string]struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	kept := map[string]struct{}{}
	for _, h := range open {
		if _, ok := c.ids[accountID][h.Order.ID]; ok || h.Order.ID == id {
			kept[h.Order.ID] = struct{}{}
		}
	}
	if len(kept) == 0 {
		delete(c.ids, accountID)
		return kept
	}
	if c.ids == nil {
		c.ids = map[string]map[string]struct{}{}
	}
	c.ids[accountID] = kept
	return kept
}

func (g *Gateway) listOrders(w http.ResponseWriter, _ *http.Request, u *User) error {
	open, err := g.ledger.OpenOrders(u.ID)
	if err != nil {
		return err
	}
	cancelling := g.cancels.sync(u.ID, open, "")
	out := make([]openOrder, len(open))
	for i, h := range open {
		_, c := cancelling[h.Order.ID]
		out[i] = newOpenOrder(h, c)
	}
	writeJSON(w, http.StatusOK, map[string]any{"orders": out})
	return nil
}

// cancelOrder asks the engine to remove one of the trader's open orders at
// the next auction, which frees what it holds. Any part that fills in that
// auction first still settles.
func (g *Gateway) cancelOrder(w http.ResponseWriter, r *http.Request, u *User) error {
	id := r.PathValue("id")
	open, err := g.ledger.OpenOrders(u.ID)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(open, func(h ledger.HoldState) bool { return h.Order.ID == id })
	if i < 0 {
		// Other traders' orders are just as unknown, so IDs can't be probed.
		return apiError{http.StatusNotFound, "unknown_order", "no open order with id " + strconv.Quote(id)}
	}
	a, err := g.ledger.Account(u.ID)
	if err != nil {
		return err
	}
	if a.Status == ledger.StatusLiquidating {
		// Liquidation is already cancelling the trader's orders, and its own
		// sells must not be.
		return ledger.ErrAccountLiquidating
	}
	g.market.Cancel(id)
	g.cancels.sync(u.ID, open, id)
	g.log.Info("order cancel requested", "order_id", id, "user_id", u.ID)
	writeJSON(w, http.StatusAccepted, newOpenOrder(open[i], true))
	return nil
}
