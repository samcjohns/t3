package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samcjohns/t3/pkg/engine"
	"github.com/samcjohns/t3/pkg/ledger"
	"github.com/samcjohns/t3/pkg/reporting"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// env is a full in-process market behind a test HTTP server.
type env struct {
	t      *testing.T
	clock  *clock
	eng    *engine.Orchestrator
	ledger *ledger.Ledger
	gw     *Gateway
	srv    *httptest.Server
}

func newEnv(t *testing.T, tweak func(*Config)) *env {
	e := &env{t: t, clock: &clock{now: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}, ledger: ledger.New()}
	reports := reporting.New(e.ledger, reporting.Config{})
	e.eng = engine.NewOrchestrator(engine.FBAMatcher{}, engine.Config{
		Clock: e.clock.Now,
		OnTick: func(tr engine.TickResult) {
			if err := e.ledger.ApplyTick(tr); err != nil {
				t.Errorf("ApplyTick: %v", err)
			}
			reports.Ingest(tr)
		},
	})
	cfg := Config{
		Symbols:            []string{"ACME", "ZED"},
		StartingCash:       100_000,
		PasswordIterations: 1,
		RateLimit:          1000,
		RateBurst:          1000,
		AllowedOrigins:     []string{"https://app.example"},
		Logger:             slog.New(slog.DiscardHandler),
		Clock:              e.clock.Now,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	e.gw = New(e.eng, e.ledger, reports, cfg)
	e.srv = httptest.NewServer(e.gw.Handler())
	t.Cleanup(e.srv.Close)
	return e
}

type response struct {
	status int
	header http.Header
	body   map[string]any
}

func (r response) errorCode() string {
	if e, ok := r.body["error"].(map[string]any); ok {
		return e["code"].(string)
	}
	return ""
}

func (e *env) do(method, path, token string, body any) response {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		buf, _ := json.Marshal(b)
		rd = bytes.NewReader(buf)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	out := response{status: res.StatusCode, header: res.Header}
	raw, _ := io.ReadAll(res.Body)
	if len(raw) > 0 && strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(raw, &out.body); err != nil {
			e.t.Fatalf("%s %s: bad JSON %q", method, path, raw)
		}
	}
	return out
}

func (e *env) expect(r response, status int) response {
	e.t.Helper()
	if r.status != status {
		e.t.Fatalf("status = %d, want %d; body %v", r.status, status, r.body)
	}
	return r
}

// trader registers and logs in a user, returning their token and account ID.
func (e *env) trader(name string) (token, id string) {
	e.t.Helper()
	creds := map[string]string{"username": name, "password": "correct horse"}
	reg := e.expect(e.do("POST", "/v1/auth/register", "", creds), http.StatusCreated)
	login := e.expect(e.do("POST", "/v1/auth/login", "", creds), http.StatusOK)
	return login.body["token"].(string), reg.body["id"].(string)
}

func (e *env) admin() string {
	e.t.Helper()
	if _, err := e.gw.CreateUser("root", "admin password", RoleAdmin); err != nil {
		e.t.Fatal(err)
	}
	login := e.expect(e.do("POST", "/v1/auth/login", "", map[string]string{"username": "root", "password": "admin password"}), http.StatusOK)
	return login.body["token"].(string)
}

func num(v any) int64 { return int64(v.(float64)) }

func TestRegisterLoginAndAccount(t *testing.T) {
	e := newEnv(t, nil)
	token, id := e.trader("alice")

	acct := e.expect(e.do("GET", "/v1/account", token, nil), http.StatusOK)
	if acct.body["id"] != id || num(acct.body["cash"]) != 100_000 {
		t.Fatalf("account = %v", acct.body)
	}

	creds := map[string]string{"username": "alice", "password": "correct horse"}
	if r := e.do("POST", "/v1/auth/register", "", creds); r.status != http.StatusConflict || r.errorCode() != "username_taken" {
		t.Fatalf("duplicate register = %d %v", r.status, r.body)
	}
	for _, bad := range []map[string]string{
		{"username": "Al", "password": "correct horse"},
		{"username": "bob", "password": "short"},
	} {
		e.expect(e.do("POST", "/v1/auth/register", "", bad), http.StatusBadRequest)
	}
	for _, bad := range []map[string]string{
		{"username": "alice", "password": "wrong password"},
		{"username": "nobody", "password": "correct horse"},
	} {
		if r := e.do("POST", "/v1/auth/login", "", bad); r.status != http.StatusUnauthorized || r.errorCode() != "invalid_credentials" {
			t.Fatalf("bad login = %d %v", r.status, r.body)
		}
	}
}

func TestAuthentication(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.TokenTTL = time.Hour })
	token, _ := e.trader("alice")

	r := e.expect(e.do("GET", "/v1/account", "", nil), http.StatusUnauthorized)
	if r.header.Get("WWW-Authenticate") != "Bearer" {
		t.Error("missing WWW-Authenticate")
	}
	e.expect(e.do("GET", "/v1/account", "forged", nil), http.StatusUnauthorized)
	e.expect(e.do("GET", "/v1/account", token, nil), http.StatusOK)

	e.clock.advance(time.Hour)
	e.expect(e.do("GET", "/v1/account", token, nil), http.StatusUnauthorized)

	token, _ = e.trader("bob")
	e.expect(e.do("POST", "/v1/auth/logout", token, nil), http.StatusNoContent)
	e.expect(e.do("GET", "/v1/account", token, nil), http.StatusUnauthorized)
}

func TestAdminOnly(t *testing.T) {
	e := newEnv(t, nil)
	token, id := e.trader("alice")
	if r := e.do("POST", "/v1/admin/accounts/"+id+"/cash", token, map[string]int64{"amount": 1}); r.status != http.StatusForbidden {
		t.Fatalf("trader deposit = %d", r.status)
	}

	admin := e.admin()
	r := e.expect(e.do("POST", "/v1/admin/accounts/"+id+"/cash", admin, map[string]int64{"amount": 5}), http.StatusOK)
	if num(r.body["cash"]) != 100_005 {
		t.Fatalf("after deposit: %v", r.body)
	}
	e.expect(e.do("POST", "/v1/admin/accounts/"+id+"/shares", admin, map[string]any{"symbol": "NOPE", "quantity": 5}), http.StatusBadRequest)
	e.expect(e.do("POST", "/v1/admin/accounts/usr_ghost/cash", admin, map[string]int64{"amount": 5}), http.StatusNotFound)
}

func TestTradingEndToEnd(t *testing.T) {
	e := newEnv(t, nil)
	admin := e.admin()
	alice, aliceID := e.trader("alice")
	bob, bobID := e.trader("bob")
	e.expect(e.do("POST", "/v1/admin/accounts/"+bobID+"/shares", admin, map[string]any{"symbol": "ACME", "quantity": 50}), http.StatusOK)

	buy := e.expect(e.do("POST", "/v1/orders", alice, map[string]any{
		"symbol": "ACME", "direction": "BUY", "type": "LIMIT", "quantity": 10, "limit_price": 1010,
	}), http.StatusAccepted)
	if buy.body["account_id"] != aliceID || buy.body["status"] != "accepted" || !strings.HasPrefix(buy.body["id"].(string), "ord_") {
		t.Fatalf("order response = %v", buy.body)
	}
	e.expect(e.do("POST", "/v1/orders", bob, map[string]any{
		"symbol": "ACME", "direction": "SELL", "type": "LIMIT", "quantity": 10, "limit_price": 1000,
	}), http.StatusAccepted)

	held := e.expect(e.do("GET", "/v1/account", alice, nil), http.StatusOK)
	if num(held.body["cash_held"]) != 10_100 {
		t.Fatalf("hold before tick = %v", held.body)
	}

	e.eng.Tick() // clears 10 @ 1005

	q := e.expect(e.do("GET", "/v1/market/ACME/quote", "", nil), http.StatusOK)
	if num(q.body["last_price"]) != 1005 {
		t.Fatalf("quote = %v", q.body)
	}
	trades := e.expect(e.do("GET", "/v1/market/ACME/trades?limit=5", "", nil), http.StatusOK)
	tape := trades.body["trades"].([]any)
	if len(tape) != 1 || strings.Contains(mustJSON(tape), aliceID) {
		t.Fatalf("public tape must not reveal accounts: %v", tape)
	}
	candles := e.expect(e.do("GET", "/v1/market/ACME/candles?interval=5m", "", nil), http.StatusOK)
	if cs := candles.body["candles"].([]any); len(cs) != 1 || num(cs[0].(map[string]any)["volume"]) != 10 {
		t.Fatalf("candles = %v", candles.body)
	}

	p := e.expect(e.do("GET", "/v1/account/portfolio", alice, nil), http.StatusOK)
	if num(p.body["cash"]) != 89_950 || num(p.body["market_value"]) != 10_050 || num(p.body["total_value"]) != 100_000 {
		t.Fatalf("portfolio = %v", p.body)
	}
	mine := e.expect(e.do("GET", "/v1/account/trades", bob, nil), http.StatusOK)
	if ts := mine.body["trades"].([]any); len(ts) != 1 || ts[0].(map[string]any)["direction"] != "SELL" {
		t.Fatalf("bob's trades = %v", mine.body)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestOrderValidation(t *testing.T) {
	e := newEnv(t, nil)
	token, _ := e.trader("alice")
	valid := `"symbol":"ACME","direction":"BUY","type":"LIMIT","quantity":1,"limit_price":100`

	cases := []struct {
		name, body string
		status     int
		code       string
	}{
		{"unknown symbol", `{"symbol":"NOPE","direction":"BUY","type":"LIMIT","quantity":1,"limit_price":100}`, 400, "invalid_order"},
		{"client account id", `{` + valid + `,"account_id":"someone-else"}`, 400, "bad_request"},
		{"client order id", `{` + valid + `,"id":"mine"}`, 400, "bad_request"},
		{"two objects", `{` + valid + `}{}`, 400, "bad_request"},
		{"malformed", `{"symbol":`, 400, "bad_request"},
		{"lowercase direction", `{"symbol":"ACME","direction":"buy","type":"LIMIT","quantity":1,"limit_price":100}`, 400, "invalid_order"},
		{"market buy without cap", `{"symbol":"ACME","direction":"BUY","type":"MARKET","quantity":1}`, 400, "invalid_order"},
		{"insufficient funds", `{"symbol":"ACME","direction":"BUY","type":"LIMIT","quantity":1000,"limit_price":1000}`, 422, "insufficient_funds"},
		{"insufficient shares", `{"symbol":"ACME","direction":"SELL","type":"LIMIT","quantity":1,"limit_price":100}`, 422, "insufficient_shares"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := e.do("POST", "/v1/orders", token, c.body)
			if r.status != c.status || r.errorCode() != c.code {
				t.Fatalf("got %d %q, want %d %q (%v)", r.status, r.errorCode(), c.status, c.code, r.body)
			}
		})
	}
	e.expect(e.do("GET", "/v1/market/ACME/trades?limit=0", "", nil), http.StatusBadRequest)
	e.expect(e.do("GET", "/v1/market/ACME/candles?interval=2m", "", nil), http.StatusBadRequest)
	e.expect(e.do("GET", "/v1/market/NOPE/quote", "", nil), http.StatusNotFound)
	if r := e.do("GET", "/v1/market/ZED/quote", "", nil); r.errorCode() != "unknown_symbol" {
		t.Fatalf("untraded quote = %v", r.body)
	}
}

type failingMarket struct{}

func (failingMarket) Submit(engine.Order) (engine.Order, error) {
	return engine.Order{}, errors.New("engine unavailable")
}

func TestEngineRejectionReleasesHold(t *testing.T) {
	l := ledger.New()
	gw := New(failingMarket{}, l, reporting.New(l, reporting.Config{}), Config{
		Symbols: []string{"ACME"}, StartingCash: 100_000, PasswordIterations: 1,
		Logger: slog.New(slog.DiscardHandler),
	})
	e := &env{t: t, gw: gw, srv: httptest.NewServer(gw.Handler())}
	t.Cleanup(e.srv.Close)
	token, id := e.trader("alice")

	r := e.do("POST", "/v1/orders", token, map[string]any{"symbol": "ACME", "direction": "BUY", "type": "LIMIT", "quantity": 1, "limit_price": 100})
	if r.status != http.StatusInternalServerError || r.body["error"].(map[string]any)["message"] != "internal error" {
		t.Fatalf("got %d %v; internal errors must not leak details", r.status, r.body)
	}
	if a, _ := l.Account(id); a.CashHeld != 0 {
		t.Fatalf("hold not released: %+v", a)
	}
}

func TestRateLimit(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.RateLimit, c.RateBurst = 1, 2 })

	e.expect(e.do("GET", "/v1/market/symbols", "", nil), http.StatusOK)
	e.expect(e.do("GET", "/v1/market/symbols", "", nil), http.StatusOK)
	r := e.expect(e.do("GET", "/v1/market/symbols", "", nil), http.StatusTooManyRequests)
	if r.header.Get("Retry-After") == "" || r.errorCode() != "rate_limited" {
		t.Fatalf("429 response = %v %v", r.header, r.body)
	}
	e.clock.advance(time.Second)
	e.expect(e.do("GET", "/v1/market/symbols", "", nil), http.StatusOK)
	e.expect(e.do("GET", "/healthz", "", nil), http.StatusOK) // never limited
}

func TestCORS(t *testing.T) {
	e := newEnv(t, nil)
	pre := func(origin string) *http.Response {
		req, _ := http.NewRequest("OPTIONS", e.srv.URL+"/v1/orders", nil)
		req.Header.Set("Origin", origin)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}
	if res := pre("https://app.example"); res.StatusCode != http.StatusNoContent || res.Header.Get("Access-Control-Allow-Origin") != "https://app.example" {
		t.Fatalf("allowed preflight = %d %v", res.StatusCode, res.Header)
	}
	if res := pre("https://evil.example"); res.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("disallowed origin got CORS headers")
	}
}

func TestMetrics(t *testing.T) {
	e := newEnv(t, nil)
	e.do("GET", "/v1/market/symbols", "", nil)
	res, err := http.Get(e.srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if want := `t3_gateway_requests_total{route="GET /v1/market/symbols",status="200"} 1`; !strings.Contains(string(body), want) {
		t.Fatalf("metrics missing %q:\n%s", want, body)
	}
}
