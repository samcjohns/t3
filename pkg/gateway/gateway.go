// Package gateway is the API gateway: the only component clients talk to. It
// authenticates and authorizes requests, rate limits them, validates them,
// translates between the public HTTP API and internal domain models, and
// routes them to the market engine, ledger and reporting service through
// ports, so those services can be in-process or remote.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/samcjohns/t3/pkg/engine"
	"github.com/samcjohns/t3/pkg/ledger"
	"github.com/samcjohns/t3/pkg/listing"
	"github.com/samcjohns/t3/pkg/reporting"
)

// Market is the market engine port.
type Market interface {
	Submit(ctx context.Context, o engine.Order) (engine.Order, error)
	LastTick() uint64
}

// Ledger is the ledger service port.
type Ledger interface {
	OpenAccount(ctx context.Context, id string) error
	Deposit(ctx context.Context, accountID string, amount int64, reference string) error
	DepositShares(ctx context.Context, accountID, symbol string, quantity int64, reference string) error
	Reserve(ctx context.Context, o engine.Order) error
	Release(ctx context.Context, orderID string) error
	Account(id string) (ledger.Account, error)
	LastTick() uint64
}

// Reports is the reporting service port.
type Reports interface {
	Quote(symbol string) (reporting.Quote, error)
	Trades(symbol string, limit int) []reporting.Trade
	AccountTrades(accountID string, limit int) []reporting.AccountTrade
	Candles(symbol string, interval time.Duration, limit int) ([]reporting.Candle, error)
	Portfolio(accountID string) (reporting.Portfolio, error)
	AccountHistory(accountID string, interval time.Duration, limit int) ([]reporting.ValueCandle, error)
	Prices() *reporting.PriceBlob
}

// Config configures a Gateway. Zero values select defaults.
type Config struct {
	// Tickers lists the tradable securities. Required.
	Tickers []listing.Ticker
	// StartingCash is credited to each newly registered trader.
	StartingCash int64
	// TokenTTL is how long a login token is valid. Default 24h.
	TokenTTL time.Duration
	// PasswordIterations is the PBKDF2 work factor. Default 600,000.
	PasswordIterations int
	// RateLimit is the sustained requests per second allowed per user, or
	// per client IP when unauthenticated. Default 10.
	RateLimit float64
	// RateBurst is the short-term burst allowed above RateLimit. Default 20.
	RateBurst int
	// AllowedOrigins lists browser origins allowed by CORS.
	AllowedOrigins []string
	// MaxLedgerLag is how many ticks the ledger may trail the engine before
	// order entry halts, since fills could no longer be settled. Default 3.
	MaxLedgerLag uint64
	// TrustedProxies are the networks whose connections may name the real
	// client in a CF-Connecting-IP header, e.g. the Docker network shared
	// with a Cloudflare tunnel connector.
	TrustedProxies []netip.Prefix
	// Users persists users and sessions. Defaults to an in-memory store.
	Users  UserStore
	Logger *slog.Logger
	Clock  func() time.Time
}

type Gateway struct {
	market  Market
	ledger  Ledger
	reports Reports
	cfg     Config
	symbols map[string]bool
	auth    *auth
	limiter *limiter
	metrics *metrics
	log     *slog.Logger
	mux     *http.ServeMux
	board   leaderboard
}

func New(market Market, ldg Ledger, reports Reports, cfg Config) *Gateway {
	if cfg.TokenTTL <= 0 {
		cfg.TokenTTL = 24 * time.Hour
	}
	if cfg.PasswordIterations <= 0 {
		cfg.PasswordIterations = 600_000
	}
	if cfg.RateLimit <= 0 {
		cfg.RateLimit = 10
	}
	if cfg.RateBurst <= 0 {
		cfg.RateBurst = 20
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.MaxLedgerLag == 0 {
		cfg.MaxLedgerLag = 3
	}
	if cfg.Users == nil {
		cfg.Users = NewMemoryUserStore()
	}
	g := &Gateway{
		market:  market,
		ledger:  ldg,
		reports: reports,
		cfg:     cfg,
		symbols: make(map[string]bool),
		auth:    newAuth(cfg.Users, cfg.PasswordIterations, cfg.TokenTTL, cfg.Clock),
		limiter: newLimiter(cfg.RateLimit, cfg.RateBurst),
		metrics: newMetrics(),
		log:     cfg.Logger,
		mux:     http.NewServeMux(),
	}
	for _, t := range cfg.Tickers {
		g.symbols[t.Symbol] = true
	}
	g.routes()
	return g
}

// access is the authorization an endpoint requires.
type access int

const (
	public access = iota
	// trader accepts a session or an API token.
	trader
	// session needs a signed-in session: an API token can trade, but cannot
	// change the credentials that control the account.
	session
	admin
)

func (g *Gateway) routes() {
	g.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	g.mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if err := g.halted(); err != nil {
			g.writeError(w, nil, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	g.mux.HandleFunc("GET /metrics", g.metrics.serve)

	g.handle("POST /v1/auth/register", public, g.register)
	g.handle("POST /v1/auth/login", public, g.login)
	g.handle("POST /v1/auth/logout", session, g.logout)

	g.handle("GET /v1/account", trader, g.getAccount)
	g.handle("GET /v1/account/profile", trader, g.getProfile)
	g.handle("POST /v1/account/password", session, g.changePassword)
	g.handle("POST /v1/account/api-token", session, g.createAPIToken)
	g.handle("DELETE /v1/account/api-token", session, g.deleteAPIToken)
	g.handle("GET /v1/account/portfolio", trader, g.getPortfolio)
	g.handle("GET /v1/account/trades", trader, g.getAccountTrades)
	g.handle("GET /v1/account/history", trader, g.getAccountHistory)
	g.handle("POST /v1/orders", trader, g.placeOrder)

	g.handle("GET /v1/leaderboard", public, g.getLeaderboard)

	g.handle("GET /v1/market/symbols", public, g.listSymbols)
	// Served outside handle: the hottest public read skips token lookup and
	// request logging.
	g.mux.HandleFunc("GET /v1/market/prices", g.servePrices)
	g.handle("GET /v1/market/{symbol}/quote", public, g.getQuote)
	g.handle("GET /v1/market/{symbol}/trades", public, g.getTrades)
	g.handle("GET /v1/market/{symbol}/candles", public, g.getCandles)

	g.handle("POST /v1/admin/accounts/{id}/cash", admin, g.depositCash)
	g.handle("POST /v1/admin/accounts/{id}/shares", admin, g.depositShares)
}

// Handler returns the gateway's HTTP handler.
func (g *Gateway) Handler() http.Handler {
	return g.cors(g.mux)
}

// CreateUser creates a user and their ledger account. Traders receive the
// configured starting cash.
func (g *Gateway) CreateUser(ctx context.Context, username, password string, role Role) (User, error) {
	u, err := g.auth.createUser(ctx, username, password, role)
	if err != nil {
		return User{}, err
	}
	if err := g.ensureAccount(ctx, u); err != nil {
		if delErr := g.auth.store.DeleteUser(ctx, u.ID); delErr != nil {
			g.log.Error("removing user after failed account setup", "user_id", u.ID, "err", delErr)
		}
		return User{}, err
	}
	return u, nil
}

// EnsureUser returns the named user, creating them (and their account) if
// they do not exist. It is for bootstrapping built-in users at startup; an
// existing user's password and role are left unchanged.
func (g *Gateway) EnsureUser(ctx context.Context, username, password string, role Role) (User, error) {
	u, err := g.CreateUser(ctx, username, password, role)
	if !errors.Is(err, ErrUsernameTaken) {
		return u, err
	}
	rec, ok, err := g.auth.store.UserByName(ctx, username)
	if err != nil {
		return User{}, err
	}
	if !ok {
		return User{}, fmt.Errorf("user %q vanished during bootstrap", username)
	}
	return rec.User, g.ensureAccount(ctx, rec.User)
}

// ensureAccount opens the user's ledger account and credits starting cash.
// Both steps are idempotent, so it also repairs a registration that crashed
// between creating the user and setting up the account.
func (g *Gateway) ensureAccount(ctx context.Context, u User) error {
	if err := g.ledger.OpenAccount(ctx, u.ID); err != nil && !errors.Is(err, ledger.ErrAccountExists) {
		return err
	}
	if u.Role == RoleTrader && g.cfg.StartingCash > 0 {
		return g.ledger.Deposit(ctx, u.ID, g.cfg.StartingCash, "starting-cash")
	}
	return nil
}

// halted reports whether order entry must stop because the ledger is too
// far behind the engine to settle new fills.
func (g *Gateway) halted() error {
	engineTick, ledgerTick := g.market.LastTick(), g.ledger.LastTick()
	if engineTick > ledgerTick+g.cfg.MaxLedgerLag {
		return apiError{http.StatusServiceUnavailable, "market_halted",
			fmt.Sprintf("trading is halted: settlement is %d ticks behind", engineTick-ledgerTick)}
	}
	return nil
}

type handlerFunc func(w http.ResponseWriter, r *http.Request, u *User) error

// handle registers h behind authentication, authorization, rate limiting,
// logging and metrics. u is nil on public routes without a valid token.
func (g *Gateway) handle(pattern string, acc access, h handlerFunc) {
	g.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		start := g.cfg.Clock()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		var user *User
		var cred credential
		err := func() error {
			token, hasToken := bearerToken(r)
			if hasToken {
				u, c, ok, err := g.auth.lookup(r.Context(), token)
				if err != nil {
					return err
				}
				if ok {
					user, cred = &u, c
				}
			}

			key := "ip:" + g.clientIP(r)
			if user != nil {
				key = "user:" + user.ID
			}
			// Internal market makers are trusted and quote every symbol each
			// tick, so they are exempt.
			if user == nil || user.Role != RoleMarketMaker {
				if err := g.rateLimit(rec, key, start); err != nil {
					return err
				}
			}

			switch {
			case acc == public:
			case user == nil:
				rec.Header().Set("WWW-Authenticate", "Bearer")
				return apiError{http.StatusUnauthorized, "unauthorized", "a valid bearer token is required"}
			case acc == admin && user.Role != RoleAdmin:
				return apiError{http.StatusForbidden, "forbidden", "admin role required"}
			case acc == session && cred != viaSession:
				return apiError{http.StatusForbidden, "forbidden", "this endpoint needs a signed-in session, not an API token"}
			}
			return h(rec, r, user)
		}()
		if err != nil {
			g.writeError(rec, r, err)
		}

		attrs := []any{"method", r.Method, "route", pattern, "status", rec.status, "duration", g.cfg.Clock().Sub(start)}
		if user != nil {
			attrs = append(attrs, "user_id", user.ID)
		}
		g.log.Info("request", attrs...)
		g.metrics.inc(fmt.Sprintf(`t3_gateway_requests_total{route=%q,status="%d"}`, pattern, rec.status))
	})
}

func (g *Gateway) rateLimit(w http.ResponseWriter, key string, now time.Time) error {
	if ok, wait := g.limiter.allow(key, now); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		return apiError{http.StatusTooManyRequests, "rate_limited", "too many requests"}
	}
	return nil
}

// servePrices writes the pre-encoded price snapshot. Clients that send the
// ETag they already have get 304 until the next tick, and caches may keep
// the response until then.
func (g *Gateway) servePrices(w http.ResponseWriter, r *http.Request) {
	now := g.cfg.Clock()
	if err := g.rateLimit(w, "ip:"+g.clientIP(r), now); err != nil {
		g.writeError(w, r, err)
		g.metrics.inc(pricesLimited)
		return
	}
	blob := g.reports.Prices()
	h := w.Header()
	h.Set("ETag", blob.ETag)
	h.Set("Access-Control-Allow-Origin", "*") // public data, no credentials
	if wait := blob.NextTickAt.Sub(now); wait > 0 {
		h.Set("Cache-Control", "public, max-age="+strconv.Itoa(int(wait/time.Second)))
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	if r.Header.Get("If-None-Match") == blob.ETag {
		w.WriteHeader(http.StatusNotModified)
		g.metrics.inc(pricesNotModified)
		return
	}
	h.Set("Content-Type", "application/json")
	w.Write(blob.JSON)
	g.metrics.inc(pricesOK)
}

const (
	pricesOK          = `t3_gateway_requests_total{route="GET /v1/market/prices",status="200"}`
	pricesNotModified = `t3_gateway_requests_total{route="GET /v1/market/prices",status="304"}`
	pricesLimited     = `t3_gateway_requests_total{route="GET /v1/market/prices",status="429"}`
)

func (g *Gateway) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && slices.Contains(g.cfg.AllowedOrigins, origin) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			if r.Method == http.MethodOptions {
				h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// apiError is an error with a public status, code and message.
type apiError struct {
	status  int
	code    string
	message string
}

func (e apiError) Error() string { return e.message }

func badRequest(format string, args ...any) error {
	return apiError{http.StatusBadRequest, "bad_request", fmt.Sprintf(format, args...)}
}

// errorMapping translates domain errors into public errors. The domain
// error's message is shown to the client, so it must not leak other users'
// data.
var errorMapping = []struct {
	err    error
	status int
	code   string
}{
	{engine.ErrInvalidOrder, http.StatusBadRequest, "invalid_order"},
	{engine.ErrDuplicateOrderID, http.StatusConflict, "duplicate_order"},
	{ledger.ErrInsufficientFunds, http.StatusUnprocessableEntity, "insufficient_funds"},
	{ledger.ErrInsufficientShares, http.StatusUnprocessableEntity, "insufficient_shares"},
	{ledger.ErrUnknownAccount, http.StatusNotFound, "unknown_account"},
	{ledger.ErrInvalidAmount, http.StatusBadRequest, "invalid_amount"},
	{reporting.ErrUnknownSymbol, http.StatusNotFound, "unknown_symbol"},
	{reporting.ErrInvalidInterval, http.StatusBadRequest, "invalid_interval"},
	{ErrUsernameTaken, http.StatusConflict, "username_taken"},
	{ErrInvalidUsername, http.StatusBadRequest, "invalid_username"},
	{ErrInvalidPassword, http.StatusBadRequest, "invalid_password"},
	{ErrInvalidCredentials, http.StatusUnauthorized, "invalid_credentials"},
}

func (g *Gateway) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var ae apiError
	if !errors.As(err, &ae) {
		ae = apiError{http.StatusInternalServerError, "internal_error", "internal error"}
		for _, m := range errorMapping {
			if errors.Is(err, m.err) {
				ae = apiError{m.status, m.code, err.Error()}
				break
			}
		}
		if ae.status == http.StatusInternalServerError {
			attrs := []any{"err", err}
			if r != nil {
				attrs = append(attrs, "method", r.Method, "path", r.URL.Path)
			}
			g.log.Error("unhandled error", attrs...)
		}
	}
	writeJSON(w, ae.status, map[string]any{"error": map[string]string{"code": ae.code, "message": ae.message}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// decodeJSON strictly decodes a single JSON object from a bounded body.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badRequest("invalid JSON body: %v", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return badRequest("body must contain a single JSON object")
	}
	return nil
}

func bearerToken(r *http.Request) (string, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return token, ok && token != ""
}

// clientIP is the address requests are attributed to. It is the connection's
// peer, unless that peer is a trusted proxy (such as a Cloudflare tunnel
// connector) that supplied the original client in CF-Connecting-IP. The
// header is ignored from anyone else, so clients cannot spoof it.
func (g *Gateway) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if len(g.cfg.TrustedProxies) == 0 {
		return host
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	peer = peer.Unmap()
	for _, p := range g.cfg.TrustedProxies {
		if p.Contains(peer) {
			if client, err := netip.ParseAddr(r.Header.Get("CF-Connecting-IP")); err == nil {
				return client.Unmap().String()
			}
			break
		}
	}
	return host
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// metrics is a minimal counter registry exposed in Prometheus text format.
type metrics struct {
	mu       sync.Mutex
	counters map[string]uint64
}

func newMetrics() *metrics { return &metrics{counters: make(map[string]uint64)} }

func (m *metrics) inc(series string) {
	m.mu.Lock()
	m.counters[series]++
	m.mu.Unlock()
}

func (m *metrics) serve(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	for _, k := range slices.Sorted(maps.Keys(m.counters)) {
		fmt.Fprintf(w, "%s %d\n", k, m.counters[k])
	}
}
