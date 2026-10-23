package gateway

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/samcjohns/t3/pkg/engine"
	"github.com/samcjohns/t3/pkg/ledger"
)

// MakerSetting is one of a market maker's tunable parameters, as the maker
// reports it.
type MakerSetting struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// Unit is "cents", "fraction", "ticks" or "" for a plain number.
	Unit string `json:"unit"`
	// Default is the maker's own value, used unless an admin overrides it.
	Default float64 `json:"default"`
	Min     float64 `json:"min"`
	Max     float64 `json:"max"`
	Integer bool    `json:"integer"`
	Help    string  `json:"help,omitempty"`
}

// valid reports whether v is an allowed value for the setting.
func (s MakerSetting) valid(v float64) bool {
	return !math.IsNaN(v) && v >= s.Min && v <= s.Max && (!s.Integer || v == math.Trunc(v))
}

// MakerControl is what admins have set for a market maker. The maker applies
// it at the start of each tick.
type MakerControl struct {
	// Paused makers place no orders and claim no news alerts.
	Paused bool `json:"paused"`
	// Overrides replace the maker's default settings, by key.
	Overrides map[string]float64 `json:"overrides"`
}

// MakerStore persists admins' controls over market makers.
type MakerStore interface {
	// MakerControl returns the zero MakerControl for a maker never set.
	MakerControl(ctx context.Context, userID string) (MakerControl, error)
	SetMakerControl(ctx context.Context, userID string, c MakerControl) error
}

// MemoryMakerStore is an in-memory MakerStore.
type MemoryMakerStore struct {
	mu   sync.Mutex
	byID map[string]MakerControl
}

func NewMemoryMakerStore() *MemoryMakerStore {
	return &MemoryMakerStore{byID: map[string]MakerControl{}}
}

func (m *MemoryMakerStore) MakerControl(_ context.Context, userID string) (MakerControl, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.byID[userID], nil
}

func (m *MemoryMakerStore) SetMakerControl(_ context.Context, userID string, c MakerControl) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byID[userID] = c
	return nil
}

// makerReports keeps what each market maker last told the gateway about
// itself. It lives in memory: makers report every tick.
type makerReports struct {
	mu   sync.Mutex
	byID map[string]makerReport
}

type makerReport struct {
	strategy string
	settings []MakerSetting
	seen     time.Time
}

func (m *makerReports) get(id string) (makerReport, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.byID[id]
	return r, ok
}

// makerOnline is how recently a maker must have synced to count as running.
const makerOnline = time.Minute

const maxMakerSettings = 64

// syncMaker records a market maker's strategy and settings, and returns what
// admins have set for it.
func (g *Gateway) syncMaker(w http.ResponseWriter, r *http.Request, u *User) error {
	if u.Role != RoleMarketMaker {
		return apiError{http.StatusForbidden, "forbidden", "only market makers sync"}
	}
	var req struct {
		Strategy string         `json:"strategy"`
		Settings []MakerSetting `json:"settings"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	if req.Strategy == "" || len(req.Strategy) > 32 {
		return badRequest("strategy must be 1-32 characters")
	}
	if len(req.Settings) > maxMakerSettings {
		return badRequest("at most %d settings", maxMakerSettings)
	}
	keys := map[string]bool{}
	for _, s := range req.Settings {
		switch {
		case s.Key == "" || len(s.Key) > 64 || keys[s.Key]:
			return badRequest("setting keys must be unique and 1-64 characters")
		case math.IsNaN(s.Min) || math.IsNaN(s.Max) || s.Min > s.Max:
			return badRequest("setting %q has an invalid range", s.Key)
		}
		keys[s.Key] = true
	}
	ctl, err := g.cfg.Makers.MakerControl(r.Context(), u.ID)
	if err != nil {
		return err
	}
	if ctl.Overrides == nil {
		ctl.Overrides = map[string]float64{}
	}
	m := &g.makers
	m.mu.Lock()
	if m.byID == nil {
		m.byID = map[string]makerReport{}
	}
	m.byID[u.ID] = makerReport{strategy: req.Strategy, settings: req.Settings, seen: g.cfg.Clock()}
	m.mu.Unlock()
	writeJSON(w, http.StatusOK, ctl)
	return nil
}

// AdminMaker is a market maker as the admin page lists it.
type AdminMaker struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	// Strategy is empty until the maker first syncs after a server start.
	Strategy string     `json:"strategy"`
	Online   bool       `json:"online"`
	SeenAt   *time.Time `json:"seen_at"`
	Paused   bool       `json:"paused"`
	// Settings are as last reported, with any admin overrides applied.
	Settings   []AdminMakerSetting `json:"settings"`
	Cash       int64               `json:"cash"`
	CashHeld   int64               `json:"cash_held"`
	TotalValue int64               `json:"total_value"`
	Holdings   []ledger.Holding    `json:"holdings"`
}

// AdminMakerSetting is a maker's setting and the value it trades with.
type AdminMakerSetting struct {
	MakerSetting
	Value      float64 `json:"value"`
	Overridden bool    `json:"overridden"`
}

func (g *Gateway) adminMaker(ctx context.Context, u User) (AdminMaker, error) {
	p, err := g.reports.Portfolio(u.ID)
	if err != nil {
		return AdminMaker{}, err
	}
	a, err := g.ledger.Account(u.ID)
	if err != nil {
		return AdminMaker{}, err
	}
	ctl, err := g.cfg.Makers.MakerControl(ctx, u.ID)
	if err != nil {
		return AdminMaker{}, err
	}
	out := AdminMaker{ID: u.ID, Username: u.Username, Paused: ctl.Paused, Settings: []AdminMakerSetting{},
		Cash: p.Cash, CashHeld: p.CashHeld, TotalValue: p.TotalValue, Holdings: a.Holdings}
	if out.Holdings == nil {
		out.Holdings = []ledger.Holding{}
	}
	if rep, ok := g.makers.get(u.ID); ok {
		out.Strategy, out.SeenAt = rep.strategy, &rep.seen
		out.Online = g.cfg.Clock().Sub(rep.seen) < makerOnline
		for _, s := range rep.settings {
			v, overridden := ctl.Overrides[s.Key]
			if !overridden {
				v = s.Default
			}
			out.Settings = append(out.Settings, AdminMakerSetting{s, v, overridden})
		}
	}
	return out, nil
}

func (g *Gateway) listMakers(w http.ResponseWriter, r *http.Request, _ *User) error {
	makers, err := g.auth.store.MarketMakers(r.Context())
	if err != nil {
		return err
	}
	out := make([]AdminMaker, 0, len(makers))
	for _, u := range makers {
		m, err := g.adminMaker(r.Context(), u)
		if errors.Is(err, ledger.ErrUnknownAccount) {
			continue // bootstrap still setting up the account
		}
		if err != nil {
			return err
		}
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"makers": out})
	return nil
}

// maker returns the market maker the request's {id} names.
func (g *Gateway) maker(r *http.Request) (User, error) {
	id := r.PathValue("id")
	makers, err := g.auth.store.MarketMakers(r.Context())
	if err != nil {
		return User{}, err
	}
	for _, u := range makers {
		if u.ID == id {
			return u, nil
		}
	}
	return User{}, apiError{http.StatusNotFound, "unknown_maker", "no market maker with id " + strconv.Quote(id)}
}

// setMakerControl pauses or resumes a market maker, or replaces its setting
// overrides. Omitted fields are left as they are.
func (g *Gateway) setMakerControl(w http.ResponseWriter, r *http.Request, u *User) error {
	var req struct {
		Paused    *bool              `json:"paused"`
		Overrides map[string]float64 `json:"overrides"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	m, err := g.maker(r)
	if err != nil {
		return err
	}
	ctl, err := g.cfg.Makers.MakerControl(r.Context(), m.ID)
	if err != nil {
		return err
	}
	if req.Paused != nil {
		ctl.Paused = *req.Paused
	}
	if req.Overrides != nil {
		rep, ok := g.makers.get(m.ID)
		if !ok && len(req.Overrides) > 0 {
			return apiError{http.StatusConflict, "maker_offline", m.Username + " hasn't connected since the server started, so its settings are unknown"}
		}
		settings := map[string]MakerSetting{}
		for _, s := range rep.settings {
			settings[s.Key] = s
		}
		for k, v := range req.Overrides {
			s, ok := settings[k]
			switch {
			case !ok:
				return badRequest("%s has no setting %q", m.Username, k)
			case !s.valid(v):
				kind := "a number"
				if s.Integer {
					kind = "a whole number"
				}
				return badRequest("%s must be %s from %g to %g", s.Label, kind, s.Min, s.Max)
			}
		}
		ctl.Overrides = req.Overrides
	}
	if err := g.cfg.Makers.SetMakerControl(r.Context(), m.ID, ctl); err != nil {
		return err
	}
	g.log.Info("admin maker control", "admin_id", u.ID, "maker_id", m.ID, "paused", ctl.Paused, "overrides", ctl.Overrides)
	out, err := g.adminMaker(r.Context(), m)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// maxForcedQuantity bounds an admin's forced order, in shares.
const maxForcedQuantity = 100_000_000

// forceMakerOrder places an order for a market maker, as if the maker had
// placed it. It defaults to IOC, and a market buy without max_cost may spend
// up to twice its quantity at the last price, or the maker's available cash.
func (g *Gateway) forceMakerOrder(w http.ResponseWriter, r *http.Request, u *User) error {
	var req orderRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	m, err := g.maker(r)
	if err != nil {
		return err
	}
	switch {
	case !g.symbols[req.Symbol]:
		return apiError{http.StatusBadRequest, "invalid_order", "unknown symbol " + strconv.Quote(req.Symbol)}
	case req.Leverage > 1:
		return badRequest("market makers can't borrow")
	case req.Quantity > maxForcedQuantity:
		return badRequest("quantity must be at most %d shares", maxForcedQuantity)
	}
	order := engine.Order{
		ID:          newID("ord"),
		AccountID:   m.ID,
		Symbol:      req.Symbol,
		Direction:   req.Direction,
		Type:        req.Type,
		Quantity:    req.Quantity,
		LimitPrice:  req.LimitPrice,
		MaxCost:     req.MaxCost,
		TimeInForce: req.TimeInForce,
	}
	if order.TimeInForce == "" {
		order.TimeInForce = engine.IOC
	}
	if order.Type == engine.Market && order.Direction == engine.Buy && order.MaxCost == 0 && order.Quantity > 0 {
		a, err := g.ledger.Account(m.ID)
		if err != nil {
			return err
		}
		if order.MaxCost = min(a.Cash-a.CashHeld, 2*order.Quantity*g.price(order.Symbol)); order.MaxCost <= 0 {
			return fmt.Errorf("%w: %s has no cash available", ledger.ErrInsufficientFunds, m.Username)
		}
	}
	accepted, err := g.submit(r.Context(), order, 1)
	if err != nil {
		return err
	}
	g.log.Info("admin forced order", "admin_id", u.ID, "maker_id", m.ID, "order_id", accepted.ID, "symbol", accepted.Symbol,
		"direction", accepted.Direction, "type", accepted.Type, "quantity", accepted.Quantity)
	writeJSON(w, http.StatusAccepted, orderResponse{Order: accepted, Leverage: 1, Status: "accepted"})
	return nil
}
