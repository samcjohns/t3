// Package gateway is the API gateway: the only component clients talk to. It
// authenticates and authorizes requests, rate limits them, validates them,
// translates between the public HTTP API and internal domain models, and
// routes them to the market engine, ledger and reporting service through
// ports, so those services can be in-process or remote.
package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/samcjohns/t3/pkg/engine"
	"github.com/samcjohns/t3/pkg/ledger"
	"github.com/samcjohns/t3/pkg/reporting"
)

// Market is the market engine port.
type Market interface {
	Submit(engine.Order) (engine.Order, error)
}

// Ledger is the ledger service port.
type Ledger interface {
	OpenAccount(id string) error
	Deposit(accountID string, amount int64) error
	DepositShares(accountID, symbol string, quantity int64) error
	Reserve(engine.Order) error
	Release(orderID string) error
	Account(id string) (ledger.Account, error)
}

// Reports is the reporting service port.
type Reports interface {
	Quote(symbol string) (reporting.Quote, error)
	Trades(symbol string, limit int) []reporting.Trade
	AccountTrades(accountID string, limit int) []reporting.AccountTrade
	Candles(symbol string, interval time.Duration, limit int) ([]reporting.Candle, error)
	Portfolio(accountID string) (reporting.Portfolio, error)
}

// Config configures a Gateway. Zero values select defaults.
type Config struct {
	// Symbols lists the tradable symbols. Required.
	Symbols []string
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
	Logger         *slog.Logger
	Clock          func() time.Time
}

type Gateway struct {
	market   Market
	ledger   Ledger
	reports  Reports
	cfg      Config
	symbols  map[string]bool
	users    *users
	sessions *sessions
	limiter  *limiter
	metrics  *metrics
	log      *slog.Logger
	mux      *http.ServeMux
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
	g := &Gateway{
		market:   market,
		ledger:   ldg,
		reports:  reports,
		cfg:      cfg,
		symbols:  make(map[string]bool),
		users:    newUsers(cfg.PasswordIterations),
		sessions: newSessions(cfg.TokenTTL, cfg.Clock),
		limiter:  newLimiter(cfg.RateLimit, cfg.RateBurst),
		metrics:  newMetrics(),
		log:      cfg.Logger,
		mux:      http.NewServeMux(),
	}
	for _, s := range cfg.Symbols {
		g.symbols[s] = true
	}
	g.routes()
	return g
}

// access is the authorization an endpoint requires.
type access int

const (
	public access = iota
	trader
	admin
)

func (g *Gateway) routes() {
	g.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	g.mux.HandleFunc("GET /metrics", g.metrics.serve)

	g.handle("POST /v1/auth/register", public, g.register)
	g.handle("POST /v1/auth/login", public, g.login)
	g.handle("POST /v1/auth/logout", trader, g.logout)

	g.handle("GET /v1/account", trader, g.getAccount)
	g.handle("GET /v1/account/portfolio", trader, g.getPortfolio)
	g.handle("GET /v1/account/trades", trader, g.getAccountTrades)
	g.handle("POST /v1/orders", trader, g.placeOrder)

	g.handle("GET /v1/market/symbols", public, g.listSymbols)
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
func (g *Gateway) CreateUser(username, password string, role Role) (User, error) {
	u, err := g.users.create(username, password, role)
	if err != nil {
		return User{}, err
	}
	if err := g.ledger.OpenAccount(u.ID); err != nil {
		g.users.remove(username)
		return User{}, err
	}
	if role == RoleTrader && g.cfg.StartingCash > 0 {
		if err := g.ledger.Deposit(u.ID, g.cfg.StartingCash); err != nil {
			return User{}, err
		}
	}
	return u, nil
}

type handlerFunc func(w http.ResponseWriter, r *http.Request, u *User) error

// handle registers h behind authentication, authorization, rate limiting,
// logging and metrics. u is nil on public routes without a valid token.
func (g *Gateway) handle(pattern string, acc access, h handlerFunc) {
	g.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		start := g.cfg.Clock()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		var user *User
		err := func() error {
			token, hasToken := bearerToken(r)
			if hasToken {
				if u, ok := g.sessions.lookup(token); ok {
					user = &u
				}
			}

			key := "ip:" + clientIP(r)
			if user != nil {
				key = "user:" + user.ID
			}
			if ok, wait := g.limiter.allow(key, start); !ok {
				rec.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
				return apiError{http.StatusTooManyRequests, "rate_limited", "too many requests"}
			}

			switch {
			case acc == public:
			case user == nil:
				rec.Header().Set("WWW-Authenticate", "Bearer")
				return apiError{http.StatusUnauthorized, "unauthorized", "a valid bearer token is required"}
			case acc == admin && user.Role != RoleAdmin:
				return apiError{http.StatusForbidden, "forbidden", "admin role required"}
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

func (g *Gateway) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && slices.Contains(g.cfg.AllowedOrigins, origin) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			if r.Method == http.MethodOptions {
				h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
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
			g.log.Error("unhandled error", "method", r.Method, "path", r.URL.Path, "err", err)
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

// clientIP is the connection's peer address. Forwarding headers are not
// trusted; a proxy in front of the gateway must be configured for that.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
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
