package engine

import (
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"
)

var seq uint64

func limit(id string, dir Direction, qty, price int64) Order {
	seq++
	return Order{ID: id, AccountID: "acct-" + id, Symbol: "ACME", Direction: dir, Type: Limit, Quantity: qty, LimitPrice: price, Sequence: seq}
}

// market builds a market order; buys get a MaxCost too large to bind.
func market(id string, dir Direction, qty int64) Order {
	seq++
	o := Order{ID: id, AccountID: "acct-" + id, Symbol: "ACME", Direction: dir, Type: Market, Quantity: qty, Sequence: seq}
	if dir == Buy {
		o.MaxCost = 1 << 40
	}
	return o
}

func ids(orders []Order) []string {
	out := []string{}
	for _, o := range orders {
		out = append(out, o.ID)
	}
	return out
}

// checkInvariants asserts properties every match must satisfy.
func checkInvariants(t *testing.T, orders []Order, r MatchResult) {
	t.Helper()
	var total int64
	for _, e := range r.Executions {
		if e.Price != r.ClearingPrice {
			t.Errorf("execution %+v not at uniform price %d", e, r.ClearingPrice)
		}
		if e.Quantity <= 0 {
			t.Errorf("non-positive execution %+v", e)
		}
		total += e.Quantity
	}
	if total != r.Volume {
		t.Errorf("executions sum to %d, volume is %d", total, r.Volume)
	}

	// Each order's fills plus remainder must equal its original quantity.
	filled := map[string]int64{}
	for _, e := range r.Executions {
		filled[e.BuyOrderID] += e.Quantity
		filled[e.SellOrderID] += e.Quantity
	}
	for _, o := range slices.Concat(r.Resting, r.Expired) {
		filled[o.ID] += o.Quantity
	}
	for _, o := range orders {
		if filled[o.ID] != o.Quantity {
			t.Errorf("order %s: fills+remainder %d, want %d", o.ID, filled[o.ID], o.Quantity)
		}
		if o.Type == Limit && r.Volume > 0 && hasFill(r, o.ID) {
			if o.Direction == Buy && o.LimitPrice < r.ClearingPrice || o.Direction == Sell && o.LimitPrice > r.ClearingPrice {
				t.Errorf("order %s filled through its limit %d at %d", o.ID, o.LimitPrice, r.ClearingPrice)
			}
		}
	}
}

func hasFill(r MatchResult, id string) bool {
	for _, e := range r.Executions {
		if e.BuyOrderID == id || e.SellOrderID == id {
			return true
		}
	}
	return false
}

func TestLimitLimitMatch(t *testing.T) {
	orders := []Order{
		limit("b1", Buy, 100, 1010),
		limit("s1", Sell, 100, 1000),
	}
	r := FBAMatcher{}.Match(orders)
	checkInvariants(t, orders, r)

	// Only tied candidates are 1000 and 1010, both volume 100 / imbalance 0.
	if r.ClearingPrice != 1005 || r.Volume != 100 {
		t.Fatalf("got price %d volume %d, want 1005/100", r.ClearingPrice, r.Volume)
	}
	want := []Execution{{Symbol: "ACME", BuyOrderID: "b1", SellOrderID: "s1", BuyAccountID: "acct-b1", SellAccountID: "acct-s1", Price: 1005, Quantity: 100}}
	if !reflect.DeepEqual(r.Executions, want) {
		t.Fatalf("executions = %+v, want %+v", r.Executions, want)
	}
	if len(r.Resting) != 0 || len(r.Expired) != 0 {
		t.Fatalf("unexpected remainders: resting %v expired %v", r.Resting, r.Expired)
	}
}

func TestMaximisesVolume(t *testing.T) {
	orders := []Order{
		limit("b1", Buy, 50, 1030),
		limit("b2", Buy, 50, 1020),
		limit("b3", Buy, 50, 1000),
		limit("s1", Sell, 60, 990),
		limit("s2", Sell, 60, 1010),
		limit("s3", Sell, 60, 1040),
	}
	r := FBAMatcher{}.Match(orders)
	checkInvariants(t, orders, r)

	// At 1010-1020 demand is 100 and supply is 120: volume 100 is the max.
	if r.Volume != 100 || r.ClearingPrice != 1015 {
		t.Fatalf("got price %d volume %d, want 1015/100", r.ClearingPrice, r.Volume)
	}
	// Sell side is oversubscribed; s1 (better price) fills first, s2 partially.
	if got := ids(r.Resting); !reflect.DeepEqual(got, []string{"b3", "s2", "s3"}) {
		t.Fatalf("resting = %v", got)
	}
	if r.Resting[1].Quantity != 20 {
		t.Fatalf("s2 remainder = %d, want 20", r.Resting[1].Quantity)
	}
}

func TestMarketAgainstPassiveLimit(t *testing.T) {
	orders := []Order{
		market("b1", Buy, 150),
		limit("s1", Sell, 100, 1000),
	}
	r := FBAMatcher{}.Match(orders)
	checkInvariants(t, orders, r)

	if r.ClearingPrice != 1000 || r.Volume != 100 {
		t.Fatalf("got price %d volume %d, want 1000/100", r.ClearingPrice, r.Volume)
	}
	if len(r.Expired) != 1 || r.Expired[0].ID != "b1" || r.Expired[0].Quantity != 50 {
		t.Fatalf("expired = %+v, want b1 x50", r.Expired)
	}
	if len(r.Resting) != 0 {
		t.Fatalf("resting = %+v, want none", r.Resting)
	}
}

func TestMarketOrdersHavePriority(t *testing.T) {
	orders := []Order{
		limit("b1", Buy, 100, 2000),
		market("b2", Buy, 100),
		limit("s1", Sell, 100, 1000),
	}
	r := FBAMatcher{}.Match(orders)
	checkInvariants(t, orders, r)

	if len(r.Executions) != 1 || r.Executions[0].BuyOrderID != "b2" {
		t.Fatalf("executions = %+v, want market buy b2 filled", r.Executions)
	}
	if got := ids(r.Resting); !reflect.DeepEqual(got, []string{"b1"}) {
		t.Fatalf("resting = %v, want [b1]", got)
	}
}

func TestMarketBuyCappedByMaxCost(t *testing.T) {
	capped := market("b1", Buy, 100)
	capped.MaxCost = 25_500 // 25 shares at 1000, with 500 to spare
	orders := []Order{capped, limit("s1", Sell, 100, 1000)}
	r := FBAMatcher{}.Match(orders)
	checkInvariants(t, orders, r)

	if r.ClearingPrice != 1000 || r.Volume != 25 {
		t.Fatalf("got price %d volume %d, want 1000/25", r.ClearingPrice, r.Volume)
	}
	if len(r.Expired) != 1 || r.Expired[0].Quantity != 75 {
		t.Fatalf("expired = %+v, want b1 x75", r.Expired)
	}
	if len(r.Resting) != 1 || r.Resting[0].Quantity != 75 {
		t.Fatalf("resting = %+v, want s1 x75", r.Resting)
	}
}

func TestMaxCostShiftsClearingPrice(t *testing.T) {
	// The market buy can afford 10 shares at 1000 but only 5 at 2000, so
	// demand falls as price rises and the higher price is no longer tied.
	capped := market("b1", Buy, 10)
	capped.MaxCost = 10_000
	orders := []Order{
		capped,
		limit("s1", Sell, 10, 1000),
		limit("b2", Buy, 1, 2000),
	}
	r := FBAMatcher{}.Match(orders)
	checkInvariants(t, orders, r)
	if r.ClearingPrice != 1000 || r.Volume != 10 {
		t.Fatalf("got price %d volume %d, want 1000/10", r.ClearingPrice, r.Volume)
	}
	for _, e := range r.Executions {
		if e.BuyOrderID == "b1" && e.Quantity*e.Price > capped.MaxCost {
			t.Fatalf("b1 spent %d, over max_cost %d", e.Quantity*e.Price, capped.MaxCost)
		}
	}
}

func TestIOCRemainderExpires(t *testing.T) {
	ioc := limit("b1", Buy, 10, 1000)
	ioc.TimeInForce = IOC
	unfilled := limit("b2", Buy, 5, 900)
	unfilled.TimeInForce = IOC
	orders := []Order{ioc, unfilled, limit("s1", Sell, 4, 1000)}
	r := FBAMatcher{}.Match(orders)
	checkInvariants(t, orders, r)
	if r.Volume != 4 || len(r.Resting) != 0 {
		t.Fatalf("got volume %d resting %+v", r.Volume, r.Resting)
	}
	if got := ids(r.Expired); !reflect.DeepEqual(got, []string{"b1", "b2"}) || r.Expired[0].Quantity != 6 {
		t.Fatalf("expired = %+v", r.Expired)
	}
}

func TestTimePriorityAtSamePrice(t *testing.T) {
	orders := []Order{
		limit("s1", Sell, 100, 1000),
		limit("b-early", Buy, 60, 1000),
		limit("b-late", Buy, 60, 1000),
	}
	r := FBAMatcher{}.Match(orders)
	checkInvariants(t, orders, r)

	if len(r.Resting) != 1 || r.Resting[0].ID != "b-late" || r.Resting[0].Quantity != 20 {
		t.Fatalf("resting = %+v, want b-late x20", r.Resting)
	}
}

func TestNoMatch(t *testing.T) {
	cases := map[string][]Order{
		"spread":      {limit("b1", Buy, 100, 990), limit("s1", Sell, 100, 1000)},
		"one side":    {limit("b1", Buy, 100, 990), limit("b2", Buy, 100, 1000)},
		"market only": {market("b1", Buy, 100), market("s1", Sell, 100)},
		"empty":       {},
	}
	for name, orders := range cases {
		t.Run(name, func(t *testing.T) {
			r := FBAMatcher{}.Match(orders)
			checkInvariants(t, orders, r)
			if r.Volume != 0 || r.ClearingPrice != 0 || len(r.Executions) != 0 {
				t.Fatalf("got %+v, want no trade", r)
			}
			if r.Executions == nil {
				t.Fatal("executions must be non-nil for stable JSON")
			}
		})
	}
}

func TestMinimisesImbalance(t *testing.T) {
	orders := []Order{
		limit("b1", Buy, 100, 1020),
		limit("b2", Buy, 30, 1000),
		limit("s1", Sell, 100, 1000),
		limit("s2", Sell, 30, 1020),
	}
	r := FBAMatcher{}.Match(orders)
	checkInvariants(t, orders, r)

	// Both 1000 and 1020 execute 100 but with imbalance 30; the midpoint
	// 1010 also executes 100 and is chosen by the final tie-break.
	if r.Volume != 100 || r.ClearingPrice != 1010 {
		t.Fatalf("got price %d volume %d, want 1010/100", r.ClearingPrice, r.Volume)
	}
}

func TestDeterministicRegardlessOfInputOrder(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	var orders []Order
	for i := range 200 {
		dir := Buy
		if i%2 == 1 {
			dir = Sell
		}
		id := string(rune('a'+i%26)) + string(rune('0'+i/26))
		if i%10 == 0 {
			orders = append(orders, market(id, dir, 1+rng.Int64N(100)))
		} else {
			orders = append(orders, limit(id, dir, 1+rng.Int64N(100), 950+rng.Int64N(100)))
		}
	}
	want := FBAMatcher{}.Match(orders)
	checkInvariants(t, orders, want)
	if want.Volume == 0 {
		t.Fatal("test batch should cross")
	}

	for range 20 {
		shuffled := slices.Clone(orders)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		before := slices.Clone(shuffled)
		got := FBAMatcher{}.Match(shuffled)
		if !reflect.DeepEqual(got, want) {
			t.Fatal("result depends on input order")
		}
		if !reflect.DeepEqual(shuffled, before) {
			t.Fatal("matcher mutated its input")
		}
	}
}
