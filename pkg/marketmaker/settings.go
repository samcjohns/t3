package marketmaker

import "math"

// Setting is one of a strategy's tunable parameters, as reported to admins.
type Setting struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// Unit is "cents", "fraction" (shown as a percentage), "ticks" or ""
	// for a plain number.
	Unit string `json:"unit"`
	// Default is the value the maker started with, used when admins have
	// not overridden it.
	Default float64 `json:"default"`
	Min     float64 `json:"min"`
	Max     float64 `json:"max"`
	Integer bool    `json:"integer"`
	Help    string  `json:"help,omitempty"`
}

// Report is what a maker tells the exchange about itself each tick.
type Report struct {
	Strategy string    `json:"strategy"`
	Settings []Setting `json:"settings"`
}

// Control is what admins have set for a maker.
type Control struct {
	// Paused makers place no orders and claim no news alerts.
	Paused bool `json:"paused"`
	// Overrides replace settings' defaults, by key.
	Overrides map[string]float64 `json:"overrides"`
}

// param binds a Setting to the strategy field it controls.
type param struct {
	Setting
	get func() float64
	set func(float64)
}

func float(key, label, unit string, p *float64, lo, hi float64, help string) param {
	return param{Setting{Key: key, Label: label, Unit: unit, Min: lo, Max: hi, Help: help},
		func() float64 { return *p }, func(v float64) { *p = v }}
}

func cents(key, label string, p *int64, lo, hi int64, help string) param {
	return param{Setting{Key: key, Label: label, Unit: "cents", Min: float64(lo), Max: float64(hi), Integer: true, Help: help},
		func() float64 { return float64(*p) }, func(v float64) { *p = int64(v) }}
}

func ticks(key, label string, p *int, lo, hi int, help string) param {
	return param{Setting{Key: key, Label: label, Unit: "ticks", Min: float64(lo), Max: float64(hi), Integer: true, Help: help},
		func() float64 { return float64(*p) }, func(v float64) { *p = int(v) }}
}

// tunable is a strategy whose parameters admins can change while it runs.
type tunable interface {
	params() []param
}

// Settings returns s's tunable settings with their current values as
// defaults, or nil if it has none.
func Settings(s Strategy) []Setting {
	t, ok := s.(tunable)
	if !ok {
		return nil
	}
	ps := t.params()
	out := make([]Setting, len(ps))
	for i, p := range ps {
		out[i] = p.Setting
		out[i].Default = p.get()
	}
	return out
}

// Configure sets each of s's settings to its override, or else its default.
// Overrides that are unknown or out of range are ignored, as are settings
// missing from defaults.
func Configure(s Strategy, defaults []Setting, overrides map[string]float64) {
	t, ok := s.(tunable)
	if !ok {
		return
	}
	def := make(map[string]float64, len(defaults))
	for _, d := range defaults {
		def[d.Key] = d.Default
	}
	for _, p := range t.params() {
		v, ok := def[p.Key]
		if !ok {
			continue
		}
		if o, ok := overrides[p.Key]; ok && p.Valid(o) {
			v = o
		}
		p.set(v)
	}
}

// Valid reports whether v is an allowed value for the setting.
func (s Setting) Valid(v float64) bool {
	return !math.IsNaN(v) && v >= s.Min && v <= s.Max && (!s.Integer || v == math.Trunc(v))
}

func (o *Opinion) params() []param {
	return []param{
		ticks("memory", "Reference memory", &o.Memory, 1, 1_000_000, "Half-life of how fast its own reference price follows the market price."),
		float("drift", "Reference drift", "fraction", &o.Drift, 0, 0.01, "Size of its own reference price's random walk each tick."),
	}
}

func (l *Liquidity) params() []param {
	return append([]param{
		cents("depth", "Depth per side", &l.Depth, 100, 100_000_000_000, "Total notional quoted on each side of each symbol."),
		float("reversion", "Reversion", "fraction", &l.Reversion, 0, 1, "How far fair value moves towards its own reference price each tick."),
		float("max_skew", "Max inventory skew", "fraction", &l.MaxSkew, 0, 0.5, "The most fair value is skewed against excess or short inventory."),
		float("max_lean", "Max lean", "fraction", &l.MaxLean, 0, 0.5, "The furthest fair value sits from the last price."),
	}, l.Opinion.params()...)
}

func (f *Flow) params() []param {
	return []param{
		float("activity", "Activity", "fraction", &f.Activity, 0, 1, "Chance of trading each symbol each tick."),
		float("aggression", "Aggression", "fraction", &f.Aggression, 0, 0.5, "How far past the price it will trade."),
		float("volatility", "Volatility", "", &f.Volatility, 0, 5, "Scale of random sentiment shocks."),
		cents("notional", "Order size", &f.Notional, 100, 10_000_000_000, "Typical order size."),
	}
}

func (m *Momentum) params() []param {
	return append([]param{
		float("fast", "Fast average", "", &m.Fast, 0.001, 1, "Smoothing factor of the fast moving average, per tick."),
		float("slow", "Slow average", "", &m.Slow, 0.001, 1, "Smoothing factor of the slow moving average, per tick."),
		float("threshold", "Threshold", "fraction", &m.Threshold, 0, 0.5, "Smallest gap between the averages it trades on."),
		float("saturation", "Saturation", "fraction", &m.Saturation, 0.0001, 1, "Gap at which it trades its full order size."),
		float("activity", "Activity", "fraction", &m.Activity, 0, 1, "Chance of trading a signalled symbol each tick."),
		float("aggression", "Aggression", "fraction", &m.Aggression, 0, 0.5, "How far past the price it will trade."),
		cents("notional", "Max order size", &m.Notional, 100, 10_000_000_000, "Largest order size."),
		float("max_deviation", "Max deviation", "fraction", &m.MaxDeviation, 0, 1, "How far from its own reference price it lets a trend carry a price."),
	}, m.Opinion.params()...)
}

func (n *News) params() []param {
	return append([]param{
		float("rate", "News rate", "fraction", &n.Rate, 0, 1, "Chance of news per symbol per tick."),
		float("min_impact", "Min impact", "fraction", &n.MinImpact, 0, 1, "Smallest move a story targets."),
		float("max_impact", "Max impact", "fraction", &n.MaxImpact, 0, 1, "Largest move a story targets."),
		ticks("min_ticks", "Min story length", &n.MinTicks, 1, 1000, "Fewest ticks it trades on one story."),
		ticks("max_ticks", "Max story length", &n.MaxTicks, 1, 1000, "Most ticks it trades on one story."),
		float("aggression", "Aggression", "fraction", &n.Aggression, 0, 0.5, "How far past the price it will trade, never past the target."),
		cents("notional", "Order size", &n.Notional, 100, 10_000_000_000, "Typical order size."),
		float("max_deviation", "Max deviation", "fraction", &n.MaxDeviation, 0.001, 0.99, "Furthest from its own reference price a story can target."),
	}, n.Opinion.params()...)
}
