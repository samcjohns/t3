package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/samcjohns/t3/internal/pgtest"
	"github.com/samcjohns/t3/pkg/engine"
)

func ioc(id, symbol string, dir engine.Direction, qty, price int64) engine.Order {
	o := limit(id, dir, qty, price)
	o.Symbol, o.TimeInForce = symbol, engine.IOC
	return o
}

// archived collects what Compact archives, by day.
type archived map[string][]engine.TickResult

func (a archived) archive(_ context.Context, day time.Time, results iter.Seq2[json.RawMessage, error]) error {
	var trs []engine.TickResult
	for raw, err := range results {
		if err != nil {
			return err
		}
		var tr engine.TickResult
		if err := json.Unmarshal(raw, &tr); err != nil {
			return err
		}
		trs = append(trs, tr)
	}
	a[day.Format(time.DateOnly)] = trs
	return nil
}

func TestCompactSlimsWholeDaysAndDeletesUnfilledOrders(t *testing.T) {
	pool, url := pgtest.NewURL(t)
	j, err := NewJournal(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	o, err := engine.OpenOrchestrator(ctx, engine.FBAMatcher{}, engine.Config{Journal: j, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}

	// Tick 1, Oct 4: b1 fills 4 against s1, which rests with 6; x1 and z1
	// expire unfilled, z1 in a book that does not trade.
	submit(t, o, limit("s1", engine.Sell, 10, 1000))
	submit(t, o, limit("b1", engine.Buy, 4, 1000))
	submit(t, o, ioc("x1", "ACME", engine.Buy, 1, 1))
	submit(t, o, ioc("z1", "ZED", engine.Sell, 1, 500))
	t1 := tick(t, o)
	// Tick 2, Oct 4: x2 fills 6 of 8, all of s1, and expires partly
	// filled; x3 expires unfilled.
	now = now.Add(time.Hour)
	submit(t, o, ioc("x2", "ACME", engine.Buy, 8, 1000))
	submit(t, o, ioc("x3", "ACME", engine.Buy, 1, 900))
	t2 := tick(t, o)
	// Tick 3, Oct 5: x4 expires unfilled, but its day is not over.
	now = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	submit(t, o, ioc("x4", "ACME", engine.Buy, 1, 1))
	t3 := tick(t, o)
	if len(t1.Books) != 2 || len(t1.Books[1].ExpiredOrderIDs) != 1 || t2.Books[0].Volume != 6 {
		t.Fatalf("unexpected setup: %+v %+v", t1, t2)
	}

	// The ledger has only applied tick 1, so Oct 4 must wait.
	arch := archived{}
	if days, err := j.Compact(ctx, now, 1, arch.archive); err != nil || len(days) != 0 || len(arch) != 0 {
		t.Fatalf("Compact before the ledger caught up = %+v, %v", days, err)
	}
	// An archive failure leaves everything in place.
	boom := errors.New("disk full")
	failing := func(context.Context, time.Time, iter.Seq2[json.RawMessage, error]) error { return boom }
	if _, err := j.Compact(ctx, now, 3, failing); !errors.Is(err, boom) {
		t.Fatalf("Compact with failing archive: %v", err)
	}
	if ticks, _ := o.TicksAfter(ctx, 0, 10); !reflect.DeepEqual(ticks, []engine.TickResult{t1, t2, t3}) {
		t.Fatalf("failed compaction changed ticks: %+v", ticks)
	}

	days, err := j.Compact(ctx, now, 3, arch.archive)
	if err != nil {
		t.Fatal(err)
	}
	want := []CompactedDay{{Day: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC), FirstTick: 1, LastTick: 2, OrdersDeleted: 3}}
	if !reflect.DeepEqual(days, want) {
		t.Fatalf("Compact = %+v, want %+v", days, want)
	}
	if got := arch["2026-10-04"]; !reflect.DeepEqual(got, []engine.TickResult{t1, t2}) {
		t.Fatalf("archived %+v", got)
	}

	ticks, err := o.TicksAfter(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	slim1, slim2 := t1, t2
	slim1.Books = []engine.BookResult{t1.Books[0]}
	slim1.Books[0].ExpiredOrderIDs = []string{}
	slim2.Books = []engine.BookResult{t2.Books[0]}
	slim2.Books[0].ExpiredOrderIDs = []string{}
	if !reflect.DeepEqual(ticks, []engine.TickResult{slim1, slim2, t3}) {
		t.Fatalf("ticks after compaction:\n got %+v\nwant %+v", ticks, []engine.TickResult{slim1, slim2, t3})
	}

	known, err := o.KnownOrders(ctx, []string{"s1", "b1", "x1", "z1", "x2", "x3", "x4"})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]bool{"s1": true, "b1": true, "x2": true, "x4": true}; !reflect.DeepEqual(known, want) {
		t.Fatalf("KnownOrders = %v, want %v", known, want)
	}

	// Compacting again finds nothing new until Oct 5 is over.
	if days, err := j.Compact(ctx, now.Add(11*time.Hour), 3, arch.archive); err != nil || len(days) != 0 {
		t.Fatalf("second Compact = %+v, %v", days, err)
	}

	// After a restart the engine keeps counting up and trades normally.
	pool.Close()
	pool2 := reconnect(t, url)
	j2, err := NewJournal(ctx, pool2)
	if err != nil {
		t.Fatal(err)
	}
	o2, err := engine.OpenOrchestrator(ctx, engine.FBAMatcher{}, engine.Config{Journal: j2, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	submit(t, o2, limit("s2", engine.Sell, 4, 1000))
	accepted, err := o2.Submit(ctx, limit("b2", engine.Buy, 4, 1000))
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Sequence <= 8 {
		t.Fatalf("sequence after restart = %d, want > 8", accepted.Sequence)
	}
	if r := tick(t, o2); r.Tick != 4 || r.Books[0].Volume != 4 {
		t.Fatalf("tick after restart = %+v", r)
	}
}

func TestCompactKeepsNewestOrder(t *testing.T) {
	pool := pgtest.New(t)
	j, err := NewJournal(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	o, err := engine.OpenOrchestrator(ctx, engine.FBAMatcher{}, engine.Config{Journal: j, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	submit(t, o, ioc("x1", "ACME", engine.Buy, 1, 1))
	submit(t, o, ioc("x2", "ACME", engine.Buy, 1, 1))
	tick(t, o)

	days, err := j.Compact(ctx, now.Add(48*time.Hour), 1, nil)
	if err != nil || len(days) != 1 || days[0].OrdersDeleted != 1 {
		t.Fatalf("Compact = %+v, %v", days, err)
	}
	if known, _ := o.KnownOrders(ctx, []string{"x1", "x2"}); known["x1"] || !known["x2"] {
		t.Fatalf("KnownOrders = %v, want only the newest kept", known)
	}
}

func reconnect(t *testing.T, url string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
