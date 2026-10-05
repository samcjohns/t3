package gateway

import (
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/samcjohns/t3/pkg/engine"
)

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (g *Gateway) register(w http.ResponseWriter, r *http.Request, _ *User) error {
	var req credentials
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	u, err := g.CreateUser(req.Username, req.Password, RoleTrader)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, u)
	return nil
}

func (g *Gateway) login(w http.ResponseWriter, r *http.Request, _ *User) error {
	var req credentials
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	u, err := g.users.authenticate(req.Username, req.Password)
	if err != nil {
		return err
	}
	token, expires := g.sessions.issue(u)
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "expires_at": expires, "user": u})
	return nil
}

func (g *Gateway) logout(w http.ResponseWriter, r *http.Request, _ *User) error {
	token, _ := bearerToken(r)
	g.sessions.revoke(token)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (g *Gateway) getAccount(w http.ResponseWriter, _ *http.Request, u *User) error {
	a, err := g.ledger.Account(u.ID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, a)
	return nil
}

func (g *Gateway) getPortfolio(w http.ResponseWriter, _ *http.Request, u *User) error {
	p, err := g.reports.Portfolio(u.ID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, p)
	return nil
}

func (g *Gateway) getAccountTrades(w http.ResponseWriter, r *http.Request, u *User) error {
	limit, err := limitParam(r)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"trades": g.reports.AccountTrades(u.ID, limit)})
	return nil
}

// orderRequest is the public order shape. ID, account and sequence are
// never taken from the client.
type orderRequest struct {
	Symbol     string           `json:"symbol"`
	Direction  engine.Direction `json:"direction"`
	Type       engine.OrderType `json:"type"`
	Quantity   int64            `json:"quantity"`
	LimitPrice int64            `json:"limit_price"`
	MaxCost    int64            `json:"max_cost"`
}

type orderResponse struct {
	engine.Order
	Status string `json:"status"`
}

func (g *Gateway) placeOrder(w http.ResponseWriter, r *http.Request, u *User) error {
	var req orderRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	if !g.symbols[req.Symbol] {
		return apiError{http.StatusBadRequest, "invalid_order", "unknown symbol " + strconv.Quote(req.Symbol)}
	}
	order := engine.Order{
		ID:         newID("ord"),
		AccountID:  u.ID,
		Symbol:     req.Symbol,
		Direction:  req.Direction,
		Type:       req.Type,
		Quantity:   req.Quantity,
		LimitPrice: req.LimitPrice,
		MaxCost:    req.MaxCost,
	}
	if err := order.Validate(); err != nil {
		return err
	}

	// Funding is reserved before the engine sees the order, so a fill can
	// never overdraw the account.
	if err := g.ledger.Reserve(order); err != nil {
		g.metrics.inc(`t3_gateway_orders_total{result="rejected"}`)
		return err
	}
	accepted, err := g.market.Submit(order)
	if err != nil {
		if relErr := g.ledger.Release(order.ID); relErr != nil {
			g.log.Error("releasing hold for rejected order", "order_id", order.ID, "err", relErr)
		}
		g.metrics.inc(`t3_gateway_orders_total{result="rejected"}`)
		return err
	}
	g.metrics.inc(`t3_gateway_orders_total{result="accepted"}`)
	g.log.Info("order accepted", "order_id", accepted.ID, "user_id", u.ID, "symbol", accepted.Symbol,
		"direction", accepted.Direction, "type", accepted.Type, "quantity", accepted.Quantity)
	writeJSON(w, http.StatusAccepted, orderResponse{Order: accepted, Status: "accepted"})
	return nil
}

func (g *Gateway) listSymbols(w http.ResponseWriter, _ *http.Request, _ *User) error {
	writeJSON(w, http.StatusOK, map[string]any{"symbols": slices.Sorted(slices.Values(g.cfg.Symbols))})
	return nil
}

func (g *Gateway) symbol(r *http.Request) (string, error) {
	s := r.PathValue("symbol")
	if !g.symbols[s] {
		return "", apiError{http.StatusNotFound, "unknown_symbol", "unknown symbol " + strconv.Quote(s)}
	}
	return s, nil
}

func (g *Gateway) getQuote(w http.ResponseWriter, r *http.Request, _ *User) error {
	sym, err := g.symbol(r)
	if err != nil {
		return err
	}
	q, err := g.reports.Quote(sym)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, q)
	return nil
}

func (g *Gateway) getTrades(w http.ResponseWriter, r *http.Request, _ *User) error {
	sym, err := g.symbol(r)
	if err != nil {
		return err
	}
	limit, err := limitParam(r)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"trades": g.reports.Trades(sym, limit)})
	return nil
}

var candleIntervals = map[string]time.Duration{
	"1m": time.Minute, "5m": 5 * time.Minute, "15m": 15 * time.Minute,
	"1h": time.Hour, "1d": 24 * time.Hour,
}

func (g *Gateway) getCandles(w http.ResponseWriter, r *http.Request, _ *User) error {
	sym, err := g.symbol(r)
	if err != nil {
		return err
	}
	limit, err := limitParam(r)
	if err != nil {
		return err
	}
	name := r.URL.Query().Get("interval")
	if name == "" {
		name = "1m"
	}
	interval, ok := candleIntervals[name]
	if !ok {
		return apiError{http.StatusBadRequest, "invalid_interval", "interval must be one of 1m, 5m, 15m, 1h, 1d"}
	}
	candles, err := g.reports.Candles(sym, interval, limit)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"interval": name, "candles": candles})
	return nil
}

func (g *Gateway) depositCash(w http.ResponseWriter, r *http.Request, u *User) error {
	var req struct {
		Amount int64 `json:"amount"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	id := r.PathValue("id")
	if err := g.ledger.Deposit(id, req.Amount); err != nil {
		return err
	}
	g.log.Info("admin cash deposit", "admin_id", u.ID, "account_id", id, "amount", req.Amount)
	return g.getAccount(w, r, &User{ID: id})
}

func (g *Gateway) depositShares(w http.ResponseWriter, r *http.Request, u *User) error {
	var req struct {
		Symbol   string `json:"symbol"`
		Quantity int64  `json:"quantity"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	if !g.symbols[req.Symbol] {
		return badRequest("unknown symbol %q", req.Symbol)
	}
	id := r.PathValue("id")
	if err := g.ledger.DepositShares(id, req.Symbol, req.Quantity); err != nil {
		return err
	}
	g.log.Info("admin share deposit", "admin_id", u.ID, "account_id", id, "symbol", req.Symbol, "quantity", req.Quantity)
	return g.getAccount(w, r, &User{ID: id})
}

const defaultLimit, maxLimit = 100, 1000

func limitParam(r *http.Request) (int, error) {
	s := r.URL.Query().Get("limit")
	if s == "" {
		return defaultLimit, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > maxLimit {
		return 0, badRequest("limit must be an integer from 1 to %d", maxLimit)
	}
	return n, nil
}
