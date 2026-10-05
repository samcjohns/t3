package engine

import (
	"cmp"
	"slices"
	"sort"
)

// FBAMatcher clears a batch with a single uniform-price call auction.
//
// The clearing price is chosen from the batch's limit prices by, in order:
//  1. maximising executable volume,
//  2. minimising the buy/sell imbalance at that price,
//  3. taking the midpoint (rounded down) of the prices still tied.
//
// Market orders take part at any price but do not set one, so a batch with no
// limit orders on either side cannot clear. A market buy fills at most as many
// shares as its MaxCost covers at the clearing price. When one side is
// oversubscribed,
// fills go by price priority (market orders first, then most aggressive
// limit) and then by sequence.
type FBAMatcher struct{}

var _ BatchMatcher = FBAMatcher{}

func (FBAMatcher) Match(orders []Order) MatchResult {
	buys, sells := partition(orders)

	price, volume := clearingPrice(buys, sells)
	res := MatchResult{ClearingPrice: price, Volume: volume, Executions: []Execution{}}
	buyFills := make([]int64, len(buys))
	sellFills := make([]int64, len(sells))
	if volume > 0 {
		buyFills = allocate(buys, volume, price)
		sellFills = allocate(sells, volume, price)
		res.Executions = pair(buys, sells, buyFills, sellFills, price)
	}
	res.Resting, res.Expired = remainders(buys, buyFills, nil, nil)
	res.Resting, res.Expired = remainders(sells, sellFills, res.Resting, res.Expired)
	return res
}

// partition copies the batch into buy and sell sides, each sorted by
// priority: market orders first, then best limit price, then sequence.
func partition(orders []Order) (buys, sells []Order) {
	for _, o := range orders {
		if o.Direction == Buy {
			buys = append(buys, o)
		} else {
			sells = append(sells, o)
		}
	}
	slices.SortFunc(buys, func(a, b Order) int {
		return priority(a, b, func(x, y int64) int { return cmp.Compare(y, x) })
	})
	slices.SortFunc(sells, func(a, b Order) int {
		return priority(a, b, cmp.Compare[int64])
	})
	return buys, sells
}

func priority(a, b Order, comparePrice func(x, y int64) int) int {
	if am, bm := a.Type == Market, b.Type == Market; am != bm {
		if am {
			return -1
		}
		return 1
	}
	if c := comparePrice(a.LimitPrice, b.LimitPrice); c != 0 {
		return c
	}
	return cmp.Compare(a.Sequence, b.Sequence)
}

// clearingPrice returns the uniform price and the volume it executes, or
// (0, 0) if the book does not cross.
func clearingPrice(buys, sells []Order) (price, volume int64) {
	var candidates []int64
	for _, o := range buys {
		if o.Type == Limit {
			candidates = append(candidates, o.LimitPrice)
		}
	}
	for _, o := range sells {
		if o.Type == Limit {
			candidates = append(candidates, o.LimitPrice)
		}
	}
	if len(candidates) == 0 {
		return 0, 0
	}
	slices.Sort(candidates)
	candidates = slices.Compact(candidates)

	demand := cumulative(buys)
	supply := cumulative(sells)
	marketBuys := sort.Search(len(buys), func(i int) bool { return buys[i].Type == Limit })

	// demandAt is the quantity willing to buy at p. Buys are sorted market
	// first then by descending limit, so willing limit buyers form a prefix
	// after the market buys, whose demand depends on p through MaxCost.
	demandAt := func(p int64) int64 {
		n := sort.Search(len(buys), func(i int) bool {
			return buys[i].Type == Limit && buys[i].LimitPrice < p
		})
		d := demand[n] - demand[marketBuys]
		for _, o := range buys[:marketBuys] {
			d += fillable(o, p)
		}
		return d
	}
	supplyAt := func(p int64) int64 {
		n := sort.Search(len(sells), func(i int) bool {
			return sells[i].Type == Limit && sells[i].LimitPrice > p
		})
		return supply[n]
	}

	var bestVolume, bestImbalance int64
	var lo, hi int64
	for _, p := range candidates {
		d, s := demandAt(p), supplyAt(p)
		v := min(d, s)
		imbalance := abs(d - s)
		switch {
		case v > bestVolume || (v == bestVolume && v > 0 && imbalance < bestImbalance):
			bestVolume, bestImbalance, lo, hi = v, imbalance, p, p
		case v == bestVolume && v > 0 && imbalance == bestImbalance:
			hi = p
		}
	}
	if bestVolume == 0 {
		return 0, 0
	}
	// Demand never rises and supply never falls as price rises, so
	// executable volume is quasi-concave in price and every price between lo
	// and hi also executes bestVolume.
	return lo + (hi-lo)/2, bestVolume
}

// cumulative returns prefix sums of quantity: out[i] is the total of the
// first i orders.
func cumulative(orders []Order) []int64 {
	out := make([]int64, len(orders)+1)
	for i, o := range orders {
		out[i+1] = out[i] + o.Quantity
	}
	return out
}

// fillable is the most an order can trade at price p.
func fillable(o Order, p int64) int64 {
	switch {
	case o.Type == Market && o.Direction == Buy:
		return min(o.Quantity, o.MaxCost/p)
	case o.Type == Market:
		return o.Quantity
	case o.Direction == Buy && o.LimitPrice < p, o.Direction == Sell && o.LimitPrice > p:
		return 0
	default:
		return o.Quantity
	}
}

// allocate fills up to volume across one side in priority order at price and
// returns each order's filled quantity.
func allocate(side []Order, volume, price int64) []int64 {
	fills := make([]int64, len(side))
	for i, o := range side {
		if volume == 0 {
			break
		}
		fills[i] = min(fillable(o, price), volume)
		volume -= fills[i]
	}
	return fills
}

// pair walks both sides in priority order, matching filled quantity into
// executions at the clearing price.
func pair(buys, sells []Order, buyFills, sellFills []int64, price int64) []Execution {
	bf, sf := slices.Clone(buyFills), slices.Clone(sellFills)
	var execs []Execution
	for i, j := 0, 0; i < len(buys) && j < len(sells); {
		if bf[i] == 0 {
			i++
			continue
		}
		if sf[j] == 0 {
			j++
			continue
		}
		qty := min(bf[i], sf[j])
		execs = append(execs, Execution{
			Symbol:        buys[i].Symbol,
			BuyOrderID:    buys[i].ID,
			SellOrderID:   sells[j].ID,
			BuyAccountID:  buys[i].AccountID,
			SellAccountID: sells[j].AccountID,
			Price:         price,
			Quantity:      qty,
		})
		bf[i] -= qty
		sf[j] -= qty
	}
	return execs
}

// remainders appends each order's unfilled quantity to resting (limit) or
// expired (market).
func remainders(side []Order, fills []int64, resting, expired []Order) ([]Order, []Order) {
	for i, o := range side {
		o.Quantity -= fills[i]
		switch {
		case o.Quantity == 0:
		case o.Type == Limit:
			resting = append(resting, o)
		default:
			expired = append(expired, o)
		}
	}
	return resting, expired
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
