// Package link implements a market maker whose prices follow outside values:
// a stock, a coin, a commodity or a town's weather. Once a day it fetches
// each linked value, turns it into a target price, and then uses deep quotes
// to pull the symbol's price slowly towards that target. Other makers keep
// trading the same symbols, so a link only shapes the long-term trend.
//
// Links are secret. The mapping file is kept out of git and the image, and
// targets appear only in this maker's own logs, never in the API.
package link

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"time"
)

// File is the mapping file's shape.
type File struct {
	Settings Settings `json:"settings"`
	Links    []Link   `json:"links"`
}

// Settings tune the maker as a whole. Zero values select defaults.
type Settings struct {
	// FetchAt is the time of day, "HH:MM" in UTC, when values are fetched.
	// Default "22:00", after US markets close.
	FetchAt string `json:"fetch_at,omitempty"`
	// Depth is the total notional quoted on each side of each linked symbol,
	// in cents. Default $300,000, three times the liquidity maker's.
	Depth int64 `json:"depth,omitempty"`
	// Band is the corridor around a link's anchor that the price may wander
	// in. The maker's bids start at its bottom and asks at its top, so the
	// walls are a backstop the price rarely reaches. Default 25%.
	Band float64 `json:"band,omitempty"`
	// Pull is the order size, in cents, the maker trades towards the anchor
	// each tick when the price is a full Band away, scaled down in proportion
	// closer in. It is a gentle bias the other makers' noise hides from tick
	// to tick but that sets the long-term trend. Default $1,000.
	Pull int64 `json:"pull,omitempty"`
	// MaxLean caps how far past the last price a wall may start, so a price
	// outside the corridor is pushed back gradually. Default 0.5% a tick.
	MaxLean float64 `json:"max_lean,omitempty"`
	// MaxDailyMove caps how far one day's target moves from the last. Default 15%.
	MaxDailyMove float64 `json:"max_daily_move,omitempty"`
}

func (s Settings) withDefaults() Settings {
	if s.FetchAt == "" {
		s.FetchAt = "22:00"
	}
	if s.Depth == 0 {
		s.Depth = 30_000_000
	}
	if s.Band == 0 {
		s.Band = 0.25
	}
	if s.Pull == 0 {
		s.Pull = 100_000
	}
	if s.MaxLean == 0 {
		s.MaxLean = 0.005
	}
	if s.MaxDailyMove == 0 {
		s.MaxDailyMove = 0.15
	}
	return s
}

func (s Settings) validate() error {
	if _, err := time.Parse("15:04", s.FetchAt); err != nil {
		return fmt.Errorf("fetch_at %q must be HH:MM", s.FetchAt)
	}
	switch {
	case s.Depth < 0:
		return fmt.Errorf("depth must be positive")
	case s.Pull < 0:
		return fmt.Errorf("pull must be positive")
	case !between(s.Band, 0, 0.5):
		return fmt.Errorf("band must be between 0 and 0.5")
	case !between(s.MaxLean, 0, 0.5):
		return fmt.Errorf("max_lean must be between 0 and 0.5")
	case !between(s.MaxDailyMove, 0, 1):
		return fmt.Errorf("max_daily_move must be between 0 and 1")
	}
	return nil
}

// lastFetch is the most recent scheduled fetch time at or before now.
func (s Settings) lastFetch(now time.Time) time.Time {
	at, _ := time.Parse("15:04", s.FetchAt)
	now = now.UTC()
	t := time.Date(now.Year(), now.Month(), now.Day(), at.Hour(), at.Minute(), 0, 0, time.UTC)
	if t.After(now) {
		t = t.AddDate(0, 0, -1)
	}
	return t
}

// Sources.
const (
	Stock     = "stock"     // Finnhub quote; Ticker is its symbol, e.g. "AAPL"
	Crypto    = "crypto"    // Coinbase spot price; Pair is e.g. "BTC-USD"
	Weather   = "weather"   // Open-Meteo daily value at Latitude, Longitude
	Commodity = "commodity" // FRED series' latest observation; Series is e.g. "DCOILWTICO"
)

// Modes turn a signal into a price.
const (
	// Ratio moves the price in proportion to the signal: target is base
	// price × (signal / base signal) ^ sensitivity. For prices.
	Ratio = "ratio"
	// Linear moves the price by sensitivity, a fraction of the base price,
	// for each unit the signal moves from its base. For values that can
	// cross zero, such as °C.
	Linear = "linear"
)

// Link maps one symbol to an outside value.
type Link struct {
	Symbol string `json:"symbol"`
	Source string `json:"source"`

	Ticker    string   `json:"ticker,omitempty"`
	Pair      string   `json:"pair,omitempty"`
	Latitude  *float64 `json:"latitude,omitempty"`
	Longitude *float64 `json:"longitude,omitempty"`
	// Variable is an Open-Meteo daily variable. Default "temperature_2m_mean".
	Variable string `json:"variable,omitempty"`
	Series   string `json:"series,omitempty"`

	// Mode is Ratio or Linear. Default Ratio, or Linear for weather.
	Mode string `json:"mode,omitempty"`
	// Sensitivity is how closely the price follows the signal; see the modes.
	// Default 1 in ratio mode; required in linear mode.
	Sensitivity float64 `json:"sensitivity,omitempty"`
	// HalfLifeDays is how long the price takes to close half the gap to a
	// new target. Default 3.
	HalfLifeDays float64 `json:"half_life_days,omitempty"`
	// BasePrice, in cents, is the price the first signal maps to. Default
	// the symbol's price when the link is first seen.
	BasePrice int64 `json:"base_price,omitempty"`
}

// key identifies the outside value. A symbol whose key changes is re-based.
func (l Link) key() string {
	switch l.Source {
	case Stock:
		return Stock + ":" + l.Ticker
	case Crypto:
		return Crypto + ":" + l.Pair
	case Weather:
		return fmt.Sprintf("%s:%g,%g:%s", Weather, *l.Latitude, *l.Longitude, l.Variable)
	default:
		return Commodity + ":" + l.Series
	}
}

func (l Link) withDefaults() Link {
	if l.Source == Weather && l.Variable == "" {
		l.Variable = "temperature_2m_mean"
	}
	if l.Mode == "" {
		l.Mode = Ratio
		if l.Source == Weather {
			l.Mode = Linear
		}
	}
	if l.Mode == Ratio && l.Sensitivity == 0 {
		l.Sensitivity = 1
	}
	if l.HalfLifeDays == 0 {
		l.HalfLifeDays = 3
	}
	return l
}

func (l Link) validate() error {
	switch {
	case l.Symbol == "":
		return fmt.Errorf("symbol is required")
	case l.Source == Stock && l.Ticker == "":
		return fmt.Errorf("stock needs a ticker")
	case l.Source == Crypto && l.Pair == "":
		return fmt.Errorf("crypto needs a pair")
	case l.Source == Weather && (l.Latitude == nil || l.Longitude == nil):
		return fmt.Errorf("weather needs a latitude and longitude")
	case l.Source == Commodity && l.Series == "":
		return fmt.Errorf("commodity needs a series")
	case l.Source != Stock && l.Source != Crypto && l.Source != Weather && l.Source != Commodity:
		return fmt.Errorf("source %q must be stock, crypto, weather or commodity", l.Source)
	case l.Mode != Ratio && l.Mode != Linear:
		return fmt.Errorf("mode %q must be ratio or linear", l.Mode)
	case l.Sensitivity == 0 || math.IsNaN(l.Sensitivity) || math.IsInf(l.Sensitivity, 0):
		return fmt.Errorf("sensitivity must be a non-zero number")
	case !(l.HalfLifeDays > 0) || math.IsInf(l.HalfLifeDays, 0):
		return fmt.Errorf("half_life_days must be positive")
	case l.BasePrice < 0:
		return fmt.Errorf("base_price must be positive")
	}
	return nil
}

// config is a parsed mapping file.
type config struct {
	settings Settings
	links    map[string]Link // by symbol
}

// load reads the mapping file. A file that can't be read or parsed is an
// error, and the caller keeps its last good config. A bad entry is reported
// in skipped, and the caller keeps that symbol's last good link if it had one.
func load(path string) (cfg config, skipped map[string]error, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return config{}, nil, err
	}
	var f File
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return config{}, nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	cfg.settings = f.Settings.withDefaults()
	if err := cfg.settings.validate(); err != nil {
		return config{}, nil, fmt.Errorf("%s settings: %w", path, err)
	}
	cfg.links = map[string]Link{}
	skipped = map[string]error{}
	for i, l := range f.Links {
		l = l.withDefaults()
		name := l.Symbol
		if name == "" {
			name = fmt.Sprintf("entry %d", i)
		}
		if _, dup := cfg.links[l.Symbol]; dup {
			skipped[name] = fmt.Errorf("symbol linked twice")
			delete(cfg.links, l.Symbol)
			continue
		}
		if err := l.validate(); err != nil {
			skipped[name] = err
			continue
		}
		if _, dup := skipped[name]; !dup {
			cfg.links[l.Symbol] = l
		}
	}
	return cfg, skipped, nil
}

func between(v, lo, hi float64) bool { return v >= lo && v <= hi }
