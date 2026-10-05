// Package listing defines the tradable securities.
package listing

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
)

// Ticker is one listed security.
type Ticker struct {
	Symbol string `json:"symbol"`
	Name   string `json:"name"`
	// ReferencePrice is the opening price in cents, used before the symbol
	// has traded and as the anchor market makers revert towards.
	ReferencePrice int64 `json:"reference_price"`
	// MakerShares is how many shares each market maker is seeded with.
	MakerShares int64 `json:"maker_shares"`
}

// Default is the example listing. All companies are fictional.
func Default() []Ticker {
	return []Ticker{
		{"ACME", "Acme Industrial Holdings", 4_250, 20_000},
		{"BYTE", "Bytewave Software", 18_740, 5_000},
		{"CRNL", "Cornell Harvest Foods", 6_310, 15_000},
		{"DYNO", "Dynomotive Robotics", 9_875, 10_000},
		{"EMBR", "Emberline Energy", 3_120, 25_000},
		{"FATH", "Fathom Deep Sea Mining", 1_465, 50_000},
		{"GLXY", "Galaxy Orbital Freight", 27_300, 4_000},
		{"HRBR", "Harborview Savings Bank", 5_590, 15_000},
		{"KILN", "Kiln & Clay Home Goods", 2_280, 30_000},
		{"ZEPH", "Zephyr Airways", 7_405, 12_000},
	}
}

var symbolPattern = regexp.MustCompile(`^[A-Z]{1,5}$`)

// Load reads a JSON array of tickers from path.
func Load(path string) ([]Ticker, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var ts []Ticker
	if err := json.Unmarshal(b, &ts); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return ts, Validate(ts)
}

// Validate checks a listing for well-formed, unique tickers.
func Validate(ts []Ticker) error {
	if len(ts) == 0 {
		return fmt.Errorf("listing is empty")
	}
	seen := map[string]bool{}
	for _, t := range ts {
		switch {
		case !symbolPattern.MatchString(t.Symbol):
			return fmt.Errorf("symbol %q must be 1-5 uppercase letters", t.Symbol)
		case seen[t.Symbol]:
			return fmt.Errorf("symbol %q listed twice", t.Symbol)
		case t.ReferencePrice <= 0:
			return fmt.Errorf("%s: reference_price must be positive", t.Symbol)
		case t.MakerShares < 0:
			return fmt.Errorf("%s: maker_shares must not be negative", t.Symbol)
		}
		seen[t.Symbol] = true
	}
	return nil
}

// Symbols returns the listing's symbols in order.
func Symbols(ts []Ticker) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Symbol
	}
	return out
}
