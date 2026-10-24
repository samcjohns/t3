package link

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Source fetches a link's current outside value.
type Source interface {
	Fetch(ctx context.Context, l Link) (float64, error)
}

// Fetcher fetches from each source's free public API:
//
//   - stock: Finnhub's quote endpoint (free key, 60 calls a minute)
//   - crypto: Coinbase's spot price (no key)
//   - weather: Open-Meteo's daily forecast API, for yesterday (no key)
//   - commodity: FRED, the St. Louis Fed's series, which carry daily energy
//     prices, monthly metal and food prices, and BLS average retail prices
//     (free key)
type Fetcher struct {
	HTTP       *http.Client
	FinnhubKey string
	FREDKey    string
	// Base URLs, overridable for tests. Empty selects the real API.
	FinnhubURL, CoinbaseURL, OpenMeteoURL, FREDURL string
}

func NewFetcher(finnhubKey, fredKey string) *Fetcher {
	return &Fetcher{HTTP: &http.Client{Timeout: 20 * time.Second}, FinnhubKey: finnhubKey, FREDKey: fredKey}
}

func (f *Fetcher) Fetch(ctx context.Context, l Link) (float64, error) {
	switch l.Source {
	case Stock:
		return f.stock(ctx, l.Ticker)
	case Crypto:
		return f.crypto(ctx, l.Pair)
	case Weather:
		return f.weather(ctx, *l.Latitude, *l.Longitude, l.Variable)
	case Commodity:
		return f.commodity(ctx, l.Series)
	}
	return 0, fmt.Errorf("unknown source %q", l.Source)
}

func (f *Fetcher) stock(ctx context.Context, ticker string) (float64, error) {
	if f.FinnhubKey == "" {
		return 0, errors.New("stock links need T3_FINNHUB_API_KEY")
	}
	var out struct {
		Current  float64 `json:"c"`
		Previous float64 `json:"pc"`
	}
	q := url.Values{"symbol": {ticker}}
	err := f.get(ctx, or(f.FinnhubURL, "https://finnhub.io")+"/api/v1/quote?"+q.Encode(),
		map[string]string{"X-Finnhub-Token": f.FinnhubKey}, &out)
	if err != nil {
		return 0, err
	}
	// Finnhub answers an unknown symbol with zeroes.
	if out.Current > 0 {
		return out.Current, nil
	}
	if out.Previous > 0 {
		return out.Previous, nil
	}
	return 0, fmt.Errorf("finnhub has no price for %q", ticker)
}

func (f *Fetcher) crypto(ctx context.Context, pair string) (float64, error) {
	var out struct {
		Data struct {
			Amount string `json:"amount"`
		} `json:"data"`
	}
	if err := f.get(ctx, or(f.CoinbaseURL, "https://api.coinbase.com")+"/v2/prices/"+url.PathEscape(pair)+"/spot", nil, &out); err != nil {
		return 0, err
	}
	return strconv.ParseFloat(out.Data.Amount, 64)
}

// weather returns variable's value for yesterday, the last whole day, in the
// location's own time zone.
func (f *Fetcher) weather(ctx context.Context, lat, lon float64, variable string) (float64, error) {
	var out struct {
		Daily map[string]json.RawMessage `json:"daily"`
	}
	q := url.Values{
		"latitude": {strconv.FormatFloat(lat, 'f', -1, 64)}, "longitude": {strconv.FormatFloat(lon, 'f', -1, 64)},
		"daily": {variable}, "past_days": {"1"}, "forecast_days": {"0"}, "timezone": {"auto"},
	}
	if err := f.get(ctx, or(f.OpenMeteoURL, "https://api.open-meteo.com")+"/v1/forecast?"+q.Encode(), nil, &out); err != nil {
		return 0, err
	}
	var values []*float64
	if err := json.Unmarshal(out.Daily[variable], &values); err != nil {
		return 0, fmt.Errorf("open-meteo %s: %w", variable, err)
	}
	for i := len(values) - 1; i >= 0; i-- {
		if values[i] != nil {
			return *values[i], nil
		}
	}
	return 0, fmt.Errorf("open-meteo has no %s for yesterday", variable)
}

// commodity returns the series' latest observation. FRED marks missing days,
// such as holidays, with ".".
func (f *Fetcher) commodity(ctx context.Context, series string) (float64, error) {
	if f.FREDKey == "" {
		return 0, errors.New("commodity links need T3_FRED_API_KEY")
	}
	var out struct {
		Observations []struct {
			Value string `json:"value"`
		} `json:"observations"`
	}
	q := url.Values{"series_id": {series}, "api_key": {f.FREDKey}, "file_type": {"json"}, "sort_order": {"desc"}, "limit": {"10"}}
	if err := f.get(ctx, or(f.FREDURL, "https://api.stlouisfed.org")+"/fred/series/observations?"+q.Encode(), nil, &out); err != nil {
		return 0, err
	}
	for _, o := range out.Observations {
		if v, err := strconv.ParseFloat(o.Value, 64); err == nil {
			return v, nil
		}
	}
	return 0, fmt.Errorf("fred has no recent value for %q", series)
}

// get decodes a JSON response. Errors leave out the URL, which may hold an
// API key.
func (f *Fetcher) get(ctx context.Context, u string, header map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return errors.New("bad request URL")
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res, err := f.HTTP.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("%s: %w", req.URL.Host, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
		return fmt.Errorf("%s: status %d", req.URL.Host, res.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("%s: %w", req.URL.Host, err)
	}
	return nil
}

// usable reports whether v is a signal the mode can use.
func usable(v float64, mode string) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && (mode != Ratio || v > 0)
}

func or(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}
