package gateway

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/samcjohns/t3/pkg/ledger"
)

// AdminUser is a trader as the admin page lists them.
type AdminUser struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	Cash       int64  `json:"cash"`
	CashHeld   int64  `json:"cash_held"`
	TotalValue int64  `json:"total_value"`
}

func (g *Gateway) listUsers(w http.ResponseWriter, r *http.Request, _ *User) error {
	traders, err := g.auth.store.Traders(r.Context())
	if err != nil {
		return err
	}
	out := make([]AdminUser, 0, len(traders))
	for _, u := range traders {
		p, err := g.reports.Portfolio(u.ID)
		if errors.Is(err, ledger.ErrUnknownAccount) {
			continue // registration still setting up the account
		}
		if err != nil {
			return err
		}
		out = append(out, AdminUser{ID: u.ID, Username: u.Username, Cash: p.Cash, CashHeld: p.CashHeld, TotalValue: p.TotalValue})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
	return nil
}

// NewsAlert asks the news market maker to market buy Quantity shares of
// Symbol, as if on its own news.
type NewsAlert struct {
	Symbol   string `json:"symbol"`
	Quantity int64  `json:"quantity"`
}

// newsAlerts queues admin news for the news market maker. The queue lives in
// memory: news that no maker claims before a restart is published but does
// not move the price.
type newsAlerts struct {
	mu     sync.Mutex
	queued []NewsAlert
}

// maxNewsAlerts bounds the queue when no news maker is running.
const maxNewsAlerts = 100

// maxNewsQuantity is the largest admin news buy, in shares.
const maxNewsQuantity = 1_000_000

// breakNews publishes an admin-written article and has the news market maker
// market buy the given number of shares of the symbol.
func (g *Gateway) breakNews(w http.ResponseWriter, r *http.Request, u *User) error {
	var req struct {
		Symbol   string `json:"symbol"`
		Quantity int64  `json:"quantity"`
		Headline string `json:"headline"`
		Body     string `json:"body"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	headline, body := strings.TrimSpace(req.Headline), strings.TrimSpace(req.Body)
	switch {
	case !g.symbols[req.Symbol]:
		return badRequest("unknown symbol %q", req.Symbol)
	case req.Quantity <= 0 || req.Quantity > maxNewsQuantity:
		return badRequest("quantity must be 1-%d shares", maxNewsQuantity)
	case headline == "" || utf8.RuneCountInString(headline) > maxHeadline:
		return badRequest("headline must be 1-%d characters", maxHeadline)
	case utf8.RuneCountInString(body) > maxBody:
		return badRequest("body must be at most %d characters", maxBody)
	}
	// Admin news has no target: how far the buy moves the price depends on
	// the book.
	a := Article{
		ID:       newID("nws"),
		Tick:     g.market.LastTick(),
		Time:     g.cfg.Clock().UTC().Truncate(time.Microsecond),
		Symbol:   req.Symbol,
		Headline: headline,
		Body:     body,
		Price:    g.price(req.Symbol),
	}
	if err := g.cfg.News.AddArticle(r.Context(), a); err != nil {
		return err
	}
	q := &g.alerts
	q.mu.Lock()
	q.queued = append(q.queued, NewsAlert{Symbol: a.Symbol, Quantity: req.Quantity})
	if n := len(q.queued); n > maxNewsAlerts {
		q.queued = q.queued[n-maxNewsAlerts:]
	}
	q.mu.Unlock()
	g.log.Info("admin news", "admin_id", u.ID, "symbol", a.Symbol, "price", a.Price, "quantity", req.Quantity)
	writeJSON(w, http.StatusCreated, a)
	return nil
}

// claimNewsAlerts hands the queued admin news to the caller, the news market
// maker, which market buys each alert's quantity.
func (g *Gateway) claimNewsAlerts(w http.ResponseWriter, _ *http.Request, _ *User) error {
	q := &g.alerts
	q.mu.Lock()
	out := q.queued
	q.queued = nil
	q.mu.Unlock()
	if out == nil {
		out = []NewsAlert{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"alerts": out})
	return nil
}

// price is a symbol's last clearing price, or its reference price if it has
// never traded.
func (g *Gateway) price(symbol string) int64 {
	if q, err := g.reports.Quote(symbol); err == nil && q.LastPrice > 0 {
		return q.LastPrice
	}
	for _, t := range g.cfg.Tickers {
		if t.Symbol == symbol {
			return t.ReferencePrice
		}
	}
	return 0
}
