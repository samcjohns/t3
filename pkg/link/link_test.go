package link

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samcjohns/t3/pkg/engine"
	"github.com/samcjohns/t3/pkg/gateway"
	"github.com/samcjohns/t3/pkg/ledger"
	"github.com/samcjohns/t3/pkg/listing"
	"github.com/samcjohns/t3/pkg/marketmaker"
	"github.com/samcjohns/t3/pkg/reporting"
)

var ctx = context.Background()

var quiet = slog.New(slog.DiscardHandler)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeSource returns values by symbol, or an error for a symbol it lacks.
type fakeSource struct {
	values map[string]float64
	calls  int
}

func (f *fakeSource) Fetch(_ context.Context, l Link) (float64, error) {
	f.calls++
	v, ok := f.values[l.Symbol]
	if !ok {
		return 0, errors.New("down")
	}
	return v, nil
}

// clock is a settable time.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newMaker(t *testing.T, file string, src Source, c *clock) *Maker {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "links.json"), file)
	m, err := New(filepath.Join(dir, "links.json"), filepath.Join(dir, "state.json"), src, 1, quiet)
	if err != nil {
		t.Fatal(err)
	}
	m.Now = c.now
	return m
}

func TestLoadKeepsLastGood(t *testing.T) {
	c := &clock{time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)}
	m := newMaker(t, `{"links": [
		{"symbol": "ACME", "source": "stock", "ticker": "AAPL"},
		{"symbol": "SUN", "source": "weather", "latitude": 51.5, "longitude": -0.12, "sensitivity": 0.02}
	]}`, &fakeSource{}, c)
	acme, sun := m.cfg.links["ACME"], m.cfg.links["SUN"]
	if acme.Mode != Ratio || acme.Sensitivity != 1 || acme.HalfLifeDays != 3 {
		t.Fatalf("ACME defaults = %+v", acme)
	}
	if sun.Mode != Linear || sun.Variable != "temperature_2m_mean" {
		t.Fatalf("SUN defaults = %+v", sun)
	}
	if m.cfg.settings.Depth != 30_000_000 || m.cfg.settings.FetchAt != "22:00" {
		t.Fatalf("settings = %+v", m.cfg.settings)
	}

	// A bad entry keeps that symbol's last good link; a new bad one is
	// skipped; a good one is added.
	write(t, m.path, `{"links": [
		{"symbol": "ACME", "source": "stock"},
		{"symbol": "SUN", "source": "weather", "latitude": 51.5, "longitude": -0.12, "sensitivity": 0.05},
		{"symbol": "OIL", "source": "commodity"},
		{"symbol": "COIN", "source": "crypto", "pair": "BTC-USD"}
	]}`)
	m.reload()
	if m.cfg.links["ACME"] != acme || m.cfg.links["SUN"].Sensitivity != 0.05 {
		t.Fatalf("links after bad entry = %+v", m.cfg.links)
	}
	if _, ok := m.cfg.links["OIL"]; ok || m.cfg.links["COIN"].Pair != "BTC-USD" {
		t.Fatalf("links = %+v", m.cfg.links)
	}

	// A malformed file, a typo'd field or bad settings keep everything.
	before := m.cfg
	for _, bad := range []string{`{"links": [`, `{"linkz": []}`, `{"settings": {"fetch_at": "25:99"}, "links": []}`} {
		write(t, m.path, bad)
		m.reload()
		if len(m.cfg.links) != len(before.links) || m.cfg.settings != before.settings {
			t.Fatalf("%s replaced the config: %+v", bad, m.cfg)
		}
	}
	os.Remove(m.path)
	m.reload()
	if len(m.cfg.links) != 3 {
		t.Fatalf("missing file dropped links: %+v", m.cfg.links)
	}

	write(t, m.path, `{"links": [
		{"symbol": "ACME", "source": "crypto", "pair": "ETH-USD"},
		{"symbol": "ACME", "source": "crypto", "pair": "BTC-USD"}
	]}`)
	m.reload()
	if _, ok := m.cfg.links["ACME"]; !ok || m.cfg.links["ACME"] != acme {
		t.Fatalf("a duplicated symbol should keep its last good link: %+v", m.cfg.links["ACME"])
	}
}

func TestLastFetch(t *testing.T) {
	s := Settings{FetchAt: "22:00"}
	cases := map[time.Time]time.Time{
		time.Date(2026, 10, 23, 21, 59, 0, 0, time.UTC): time.Date(2026, 10, 22, 22, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 23, 22, 0, 0, 0, time.UTC):  time.Date(2026, 10, 23, 22, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 24, 3, 0, 0, 0, time.UTC):   time.Date(2026, 10, 23, 22, 0, 0, 0, time.UTC),
	}
	for now, want := range cases {
		if got := s.lastFetch(now); !got.Equal(want) {
			t.Errorf("lastFetch(%v) = %v, want %v", now, got, want)
		}
	}
}

func TestFetchSchedule(t *testing.T) {
	c := &clock{time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)}
	src := &fakeSource{values: map[string]float64{"ACME": 100}}
	m := newMaker(t, `{"links": [
		{"symbol": "ACME", "source": "stock", "ticker": "AAPL"},
		{"symbol": "ZED", "source": "stock", "ticker": "BAD"}
	]}`, src, c)

	m.fetchDue(ctx) // both new, so both due
	if src.calls != 2 || m.state["ACME"].Signal != 100 {
		t.Fatalf("calls %d, state %+v", src.calls, m.state)
	}
	c.t = c.t.Add(30 * time.Minute)
	m.fetchDue(ctx) // ACME fetched; ZED failed under an hour ago
	if src.calls != 2 {
		t.Fatalf("calls %d, want 2", src.calls)
	}
	c.t = c.t.Add(31 * time.Minute)
	m.fetchDue(ctx) // ZED retries
	if src.calls != 3 {
		t.Fatalf("calls %d, want 3", src.calls)
	}
	delete(src.values, "ACME")
	src.values["ZED"] = 5
	c.t = time.Date(2026, 10, 23, 22, 0, 0, 0, time.UTC)
	m.fetchDue(ctx) // the daily fetch: both due
	if src.calls != 5 || m.state["ZED"].Signal != 5 || m.state["ACME"].Signal != 100 {
		t.Fatalf("calls %d, state %+v %+v", src.calls, m.state["ACME"], m.state["ZED"])
	}

	// State survives a restart.
	m2, err := New(m.path, m.statePath, src, 2, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if m2.state["ZED"].Signal != 5 || !m2.state["ZED"].Fresh {
		t.Fatalf("restored state %+v", m2.state["ZED"])
	}

	// Junk values are rejected for prices, but zero is a fine temperature.
	if usable(0, Ratio) || usable(math.NaN(), Linear) || !usable(0, Linear) || !usable(-3, Linear) {
		t.Fatal("usable")
	}
}

func snap(prices map[string]int64) marketmaker.Snapshot {
	var s marketmaker.Snapshot
	for sym, p := range prices {
		s.Prices = append(s.Prices, marketmaker.Price{Symbol: sym, Price: p})
	}
	return s
}

func TestTargets(t *testing.T) {
	c := &clock{time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)}
	src := &fakeSource{values: map[string]float64{"ACME": 100, "SUN": 10, "ZED": 50}}
	m := newMaker(t, `{"links": [
		{"symbol": "ACME", "source": "stock", "ticker": "AAPL"},
		{"symbol": "SUN", "source": "weather", "latitude": 1, "longitude": 2, "sensitivity": 0.01},
		{"symbol": "ZED", "source": "crypto", "pair": "Z-USD", "base_price": 2000}
	]}`, src, c)

	// No orders before anything is fetched.
	if o := m.Orders(snap(map[string]int64{"ACME": 1000, "SUN": 1000, "ZED": 1000}), marketmaker.Account{Cash: 1e9}); len(o) != 0 {
		t.Fatalf("orders before a fetch: %v", o)
	}
	m.fetchDue(ctx)
	m.Orders(snap(map[string]int64{"ACME": 1000, "SUN": 1000, "ZED": 1000}), marketmaker.Account{})
	if st := m.state["ACME"]; st.BasePrice != 1000 || st.BaseSignal != 100 || st.Target != 1000 || st.Anchor != 1000 {
		t.Fatalf("ACME based at %+v", st)
	}
	if st := m.state["ZED"]; st.BasePrice != 2000 || st.Target != 2000 || st.Anchor != 1000 {
		t.Fatalf("ZED based at %+v", st)
	}

	// Next day: ACME's value doubles, but the target moves 15% at most;
	// SUN warms 5 °C, so its target rises 5%.
	src.values["ACME"], src.values["SUN"] = 200, 15
	c.t = c.t.Add(24 * time.Hour)
	m.fetchDue(ctx)
	m.Orders(snap(map[string]int64{"ACME": 1000, "SUN": 1000}), marketmaker.Account{})
	if got := m.state["ACME"].Target; math.Abs(got-1150) > 1e-9 {
		t.Fatalf("ACME target %v, want 1150", got)
	}
	if got := m.state["SUN"].Target; math.Abs(got-1050) > 1e-9 {
		t.Fatalf("SUN target %v, want 1050", got)
	}
	c.t = c.t.Add(24 * time.Hour)
	m.fetchDue(ctx)
	m.Orders(snap(map[string]int64{"ACME": 1000}), marketmaker.Account{})
	if got := m.state["ACME"].Target; math.Abs(got-1150*1.15) > 1e-9 {
		t.Fatalf("ACME day-two target %v, want %v", got, 1150*1.15)
	}

	// A new source re-bases the symbol at the current price.
	write(t, m.path, `{"links": [{"symbol": "ACME", "source": "crypto", "pair": "BTC-USD"}]}`)
	m.reload()
	src.values["ACME"] = 7
	m.fetchDue(ctx)
	m.Orders(snap(map[string]int64{"ACME": 1234}), marketmaker.Account{})
	if st := m.state["ACME"]; st.BasePrice != 1234 || st.BaseSignal != 7 || st.Target != 1234 {
		t.Fatalf("ACME re-based at %+v", st)
	}
}

func TestOrdersLeanAndStayFunded(t *testing.T) {
	c := &clock{time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)}
	src := &fakeSource{values: map[string]float64{"ACME": 100}}
	m := newMaker(t, `{"links": [{"symbol": "ACME", "source": "stock", "ticker": "AAPL", "base_price": 2000}]}`, src, c)
	m.fetchDue(ctx)
	acct := marketmaker.Account{Cash: 5_000_000, Holdings: []struct {
		Symbol   string `json:"symbol"`
		Quantity int64  `json:"quantity"`
		Held     int64  `json:"held"`
	}{{"ACME", 50, 0}}}
	m.Orders(snap(map[string]int64{"ACME": 1000, "ZED": 1000}), acct)
	c.t = c.t.Add(time.Hour) // counts as a minute
	orders := m.Orders(snap(map[string]int64{"ACME": 1000, "ZED": 1000}), acct)
	if a := m.state["ACME"].Anchor; a <= 1000 || a >= 1001 {
		t.Fatalf("anchor %v after a minute; want a slow move up from 1000", a)
	}

	var spent, sold int64
	bid, ask := int64(0), int64(math.MaxInt64)
	for _, o := range orders {
		if o.Symbol != "ACME" || o.TimeInForce != "IOC" {
			t.Fatalf("bad order %+v", o)
		}
		if o.Direction == "BUY" {
			spent += o.Quantity * o.LimitPrice
			bid = max(bid, o.LimitPrice)
		} else {
			sold += o.Quantity
			ask = min(ask, o.LimitPrice)
		}
	}
	if spent > 5_000_000 || sold > 50 {
		t.Fatalf("spent %d, sold %d", spent, sold)
	}
	if bid >= 1000 || ask <= 1000 {
		t.Fatalf("quotes %d/%d; want them around 1000", bid, ask)
	}

	// Far below its anchor, bids lean up, but no further than max_lean.
	m.state["ACME"].Anchor = 1e6
	for _, o := range m.Orders(snap(map[string]int64{"ACME": 1000}), acct) {
		if o.Direction == "BUY" && (o.LimitPrice > 1005 || o.LimitPrice < 1000*0.97) {
			t.Fatalf("bid %d leans past max_lean", o.LimitPrice)
		}
		if o.Direction == "SELL" && o.LimitPrice <= 1005 {
			t.Fatalf("ask %d; want it at the top of the corridor", o.LimitPrice)
		}
	}
}

func TestFetcher(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case r.URL.Path == "/api/v1/quote" && r.Header.Get("X-Finnhub-Token") == "fk" && q.Get("symbol") == "AAPL":
			fmt.Fprint(w, `{"c": 251.3, "pc": 249}`)
		case r.URL.Path == "/api/v1/quote":
			fmt.Fprint(w, `{"c": 0, "pc": 0}`)
		case r.URL.Path == "/v2/prices/BTC-USD/spot":
			fmt.Fprint(w, `{"data": {"amount": "80555.81", "base": "BTC", "currency": "USD"}}`)
		case r.URL.Path == "/v1/forecast" && q.Get("daily") == "temperature_2m_mean" && q.Get("past_days") == "1":
			fmt.Fprint(w, `{"daily": {"time": ["2026-10-22"], "temperature_2m_mean": [-1.5]}}`)
		case r.URL.Path == "/fred/series/observations" && q.Get("api_key") == "rk" && q.Get("series_id") == "DCOILWTICO":
			fmt.Fprint(w, `{"observations": [{"date": "2026-10-23", "value": "."}, {"date": "2026-10-22", "value": "71.25"}]}`)
		default:
			http.Error(w, "nope", http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	f := &Fetcher{HTTP: srv.Client(), FinnhubKey: "fk", FREDKey: "rk",
		FinnhubURL: srv.URL, CoinbaseURL: srv.URL, OpenMeteoURL: srv.URL, FREDURL: srv.URL}
	lat, lon := 51.5, -0.12
	for _, c := range []struct {
		l    Link
		want float64
	}{
		{Link{Source: Stock, Ticker: "AAPL"}, 251.3},
		{Link{Source: Crypto, Pair: "BTC-USD"}, 80555.81},
		{Link{Source: Weather, Latitude: &lat, Longitude: &lon, Variable: "temperature_2m_mean"}, -1.5},
		{Link{Source: Commodity, Series: "DCOILWTICO"}, 71.25},
	} {
		if got, err := f.Fetch(ctx, c.l); err != nil || got != c.want {
			t.Errorf("%s = %v, %v; want %v", c.l.Source, got, err, c.want)
		}
	}
	if _, err := f.Fetch(ctx, Link{Source: Stock, Ticker: "NOPE"}); err == nil {
		t.Error("unknown stock fetched")
	}
	if _, err := f.Fetch(ctx, Link{Source: Commodity, Series: "NOPE"}); err == nil || strings.Contains(err.Error(), "rk") {
		t.Errorf("bad series error %v; want one without the key", err)
	}
	f.FREDKey = ""
	if _, err := f.Fetch(ctx, Link{Source: Commodity, Series: "DCOILWTICO"}); err == nil {
		t.Error("fetched without a key")
	}
}

var makers = []string{"mm-liquidity", "mm-flow", "mm-momentum", "mm-news", "mm-link"}

// TestLinkPullsThePrice runs the link maker beside the other four makers on
// a real in-process exchange, links one symbol to a value that rises 10%,
// and checks its price follows while another symbol doesn't trend.
func TestLinkPullsThePrice(t *testing.T) {
	tickers := listing.Default()
	ldg := ledger.New()
	reports := reporting.New(ldg, reporting.Config{Listing: tickers, Heartbeat: 10 * time.Second})
	eng := engine.NewOrchestrator(engine.FBAMatcher{}, engine.Config{OnTick: func(tr engine.TickResult) {
		if err := ldg.ApplyTick(ctx, tr); err != nil {
			t.Errorf("ApplyTick: %v", err)
		}
		reports.Ingest(tr)
	}})
	gw := gateway.New(eng, ldg, reports, gateway.Config{Tickers: tickers, StartingCash: 1_000_000, PasswordIterations: 1, RateLimit: 1e6, RateBurst: 1e6, Logger: quiet})
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()
	for _, name := range makers {
		u, err := gw.EnsureUser(ctx, name, "maker password", gateway.RoleMarketMaker)
		if err != nil {
			t.Fatal(err)
		}
		cash := int64(500_000_000)
		if name == "mm-link" {
			cash = 2_500_000_000
		}
		if err := ldg.Deposit(ctx, u.ID, cash, "seed-cash"); err != nil {
			t.Fatal(err)
		}
		for _, tk := range tickers {
			if err := ldg.DepositShares(ctx, u.ID, tk.Symbol, tk.MakerShares, "seed:"+tk.Symbol); err != nil {
				t.Fatal(err)
			}
		}
	}

	linked, other := tickers[0].Symbol, tickers[1].Symbol
	c := &clock{time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)}
	src := &fakeSource{values: map[string]float64{linked: 100}}
	lm := newMaker(t, fmt.Sprintf(`{"links": [{"symbol": %q, "source": "crypto", "pair": "X-USD", "half_life_days": 0.02}]}`, linked), src, c)
	lm.fetchDue(ctx)

	type running struct {
		c *marketmaker.HTTPClient
		s marketmaker.Strategy
	}
	run := []running{
		{marketmaker.NewHTTPClient(srv.URL, "mm-liquidity", "maker password"), marketmaker.NewLiquidity(7)},
		{marketmaker.NewHTTPClient(srv.URL, "mm-flow", "maker password"), marketmaker.NewFlow(8)},
		{marketmaker.NewHTTPClient(srv.URL, "mm-momentum", "maker password"), marketmaker.NewMomentum(9)},
		{marketmaker.NewHTTPClient(srv.URL, "mm-news", "maker password"), marketmaker.NewNews(10)},
		{marketmaker.NewHTTPClient(srv.URL, "mm-link", "maker password"), lm},
	}
	price := func() map[string]int64 {
		s, err := run[0].c.Prices(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int64{}
		for _, p := range s.Prices {
			out[p.Symbol] = p.Price
		}
		return out
	}
	tick := func(n int) {
		for range n {
			s, err := run[0].c.Prices(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range run {
				acct, err := r.c.Account(ctx)
				if err != nil {
					t.Fatal(err)
				}
				for _, o := range r.s.Orders(s, acct) {
					r.c.PlaceOrder(ctx, o)
				}
			}
			if _, err := eng.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			c.t = c.t.Add(10 * time.Second)
		}
	}

	tick(1)
	start := price()
	before, err := run[4].c.Account(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if lm.state[linked].BasePrice != float64(start[linked]) && lm.state[linked].BasePrice == 0 {
		t.Fatalf("link not based: %+v", lm.state[linked])
	}
	tick(200)
	// The value rises 10%; with a half-life of about half an hour, the
	// anchor closes most of the gap in a few hours.
	src.values[linked] = 110
	c.t = c.t.Add(24 * time.Hour)
	lm.fetchDue(ctx)
	tick(1500)
	end := price()

	target := lm.state[linked].Target
	got := float64(end[linked]) / float64(start[linked])
	t.Logf("%s: %d -> %d (target %.0f); %s: %d -> %d", linked, start[linked], end[linked], target, other, start[other], end[other])
	if math.Abs(target/lm.state[linked].BasePrice-1.1) > 1e-9 {
		t.Fatalf("target %v; want 10%% over base %v", target, lm.state[linked].BasePrice)
	}
	// The anchor has all but reached the target, and the price is inside
	// the corridor around it, give or take a wall's first level.
	band := lm.cfg.settings.Band + 0.005
	if p := float64(end[linked]); p < target*(1-band) || p > target*(1+band) {
		t.Errorf("%s moved %.1f%% to %.0f; want within %.1f%% of %.0f", linked, (got-1)*100, p, band*100, target)
	}

	// Pushing costs the link maker little: at today's prices, its account is
	// worth about what its starting one is.
	after, err := run[4].c.Account(ctx)
	if err != nil {
		t.Fatal(err)
	}
	worth := func(a marketmaker.Account) float64 {
		v := a.Cash
		for _, h := range a.Holdings {
			v += h.Quantity * end[h.Symbol]
		}
		return float64(v)
	}
	pnl := worth(after)/worth(before) - 1
	t.Logf("link maker: %+.3f%% traded %d -> %d cash", pnl*100, before.Cash, after.Cash)
	if pnl < -0.01 {
		t.Errorf("link maker lost %.2f%% pushing the price", -pnl*100)
	}
}
