package gateway

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Article is a market news story. The news market maker publishes one each
// time it starts trading a symbol towards a new target.
type Article struct {
	ID       string    `json:"id"`
	Tick     uint64    `json:"tick"`
	Time     time.Time `json:"time"`
	Symbol   string    `json:"symbol"`
	Headline string    `json:"headline"`
	Body     string    `json:"body,omitempty"`
	// Price is the symbol's price when the story broke, and Target the
	// price the story moves it towards.
	Price  int64 `json:"price"`
	Target int64 `json:"target"`
}

// NewsStore persists articles.
type NewsStore interface {
	AddArticle(ctx context.Context, a Article) error
	// Articles returns up to limit articles, newest first, only for symbol
	// unless it is empty.
	Articles(ctx context.Context, symbol string, limit int) ([]Article, error)
}

const maxHeadline, maxBody = 200, 4000

func (g *Gateway) publishNews(w http.ResponseWriter, r *http.Request, _ *User) error {
	var req struct {
		Symbol   string `json:"symbol"`
		Headline string `json:"headline"`
		Body     string `json:"body"`
		Price    int64  `json:"price"`
		Target   int64  `json:"target"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	a := Article{
		ID:       newID("nws"),
		Tick:     g.market.LastTick(),
		Time:     g.cfg.Clock().UTC().Truncate(time.Microsecond),
		Symbol:   req.Symbol,
		Headline: strings.TrimSpace(req.Headline),
		Body:     strings.TrimSpace(req.Body),
		Price:    req.Price,
		Target:   req.Target,
	}
	switch {
	case !g.symbols[a.Symbol]:
		return badRequest("unknown symbol %q", a.Symbol)
	case a.Headline == "" || utf8.RuneCountInString(a.Headline) > maxHeadline:
		return badRequest("headline must be 1-%d characters", maxHeadline)
	case utf8.RuneCountInString(a.Body) > maxBody:
		return badRequest("body must be at most %d characters", maxBody)
	case a.Price <= 0 || a.Target <= 0:
		return badRequest("price and target must be positive cents")
	}
	if err := g.cfg.News.AddArticle(r.Context(), a); err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, a)
	return nil
}

func (g *Gateway) getNews(w http.ResponseWriter, r *http.Request, _ *User) error {
	limit, err := limitParam(r)
	if err != nil {
		return err
	}
	sym := r.URL.Query().Get("symbol")
	if sym != "" && !g.symbols[sym] {
		return apiError{http.StatusNotFound, "unknown_symbol", "unknown symbol " + strconv.Quote(sym)}
	}
	articles, err := g.cfg.News.Articles(r.Context(), sym, limit)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"articles": articles})
	return nil
}

// MemoryNewsStore is an in-memory NewsStore that keeps only the newest
// articles.
type MemoryNewsStore struct {
	mu       sync.RWMutex
	articles []Article // oldest first
}

// memoryNewsCap bounds the memory store, which serves at most maxLimit.
const memoryNewsCap = 2 * maxLimit

func NewMemoryNewsStore() *MemoryNewsStore { return &MemoryNewsStore{} }

func (m *MemoryNewsStore) AddArticle(_ context.Context, a Article) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.articles = append(m.articles, a)
	if n := len(m.articles); n > memoryNewsCap {
		m.articles = slices.Clone(m.articles[n-memoryNewsCap:])
	}
	return nil
}

func (m *MemoryNewsStore) Articles(_ context.Context, symbol string, limit int) ([]Article, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []Article{}
	for i := len(m.articles) - 1; i >= 0 && len(out) < limit; i-- {
		if a := m.articles[i]; symbol == "" || a.Symbol == symbol {
			out = append(out, a)
		}
	}
	return out, nil
}
