package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"time"

	"github.com/jackc/pgx/v5"
)

// ArchiveFunc stores one UTC day's original tick results, in tick order,
// before Compact slims them. It must have stored them durably before
// returning nil. If a compaction fails after archiving, the next one archives
// the same day again, so a second call for a day must replace the first.
type ArchiveFunc func(ctx context.Context, day time.Time, results iter.Seq2[json.RawMessage, error]) error

// CompactedDay describes one day that Compact compacted.
type CompactedDay struct {
	Day                 time.Time
	FirstTick, LastTick uint64
	OrdersDeleted       int64
}

// Compact shrinks history that nothing needs in full any more, one whole UTC
// day at a time, oldest first. A day is compacted once it ended at or before
// cutoff and every one of its ticks is at or before maxTick, the newest tick
// that consumers of expired order IDs (the ledger) have applied. For each day:
//
//   - archive, unless nil, receives the day's original tick results;
//   - each tick result keeps only the books that traded, with their expired
//     order IDs emptied. Every price, volume and execution is unchanged;
//   - orders that closed by then without filling at all are deleted.
//
// It stops at the first day that is not yet eligible, and returns the days it
// compacted, even when it also returns an error.
func (j *Journal) Compact(ctx context.Context, cutoff time.Time, maxTick uint64, archive ArchiveFunc) ([]CompactedDay, error) {
	var out []CompactedDay
	for {
		d, ok, err := j.compactDay(ctx, cutoff, maxTick, archive)
		if err != nil || !ok {
			return out, err
		}
		out = append(out, d)
	}
}

func (j *Journal) compactDay(ctx context.Context, cutoff time.Time, maxTick uint64, archive ArchiveFunc) (CompactedDay, bool, error) {
	var d CompactedDay
	var through uint64
	if err := j.pool.QueryRow(ctx, `SELECT through_tick FROM engine.compaction`).Scan(&through); err != nil {
		return d, false, err
	}
	var first time.Time
	err := j.pool.QueryRow(ctx, `SELECT tick, timestamp FROM engine.tick_results WHERE tick > $1 ORDER BY tick LIMIT 1`, through).
		Scan(&d.FirstTick, &first)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, false, nil
	} else if err != nil {
		return d, false, err
	}
	d.Day = first.UTC().Truncate(24 * time.Hour)
	end := d.Day.Add(24 * time.Hour)
	if end.After(cutoff) {
		return d, false, nil
	}
	if err := j.pool.QueryRow(ctx, `SELECT max(tick) FROM engine.tick_results WHERE tick > $1 AND timestamp < $2`, through, end).
		Scan(&d.LastTick); err != nil {
		return d, false, err
	}
	if d.LastTick > maxTick {
		return d, false, nil
	}

	if archive != nil {
		if err := archive(ctx, d.Day, j.results(ctx, through, d.LastTick)); err != nil {
			return d, false, fmt.Errorf("archiving %s: %w", d.Day.Format(time.DateOnly), err)
		}
	}
	err = pgx.BeginFunc(ctx, j.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE engine.tick_results
			SET result = jsonb_set(result, '{books}', coalesce((
				SELECT jsonb_agg(jsonb_set(b, '{expired_order_ids}', '[]'::jsonb) ORDER BY i)
				FROM jsonb_array_elements(result->'books') WITH ORDINALITY AS books(b, i)
				WHERE (b->>'volume')::bigint > 0), '[]'::jsonb))
			WHERE tick > $1 AND tick <= $2`, through, d.LastTick); err != nil {
			return err
		}
		// The newest order is kept, so the sequence counter, recovered as
		// max(sequence), never goes backwards.
		tag, err := tx.Exec(ctx, `
			DELETE FROM engine.orders
			WHERE status = 'expired' AND remaining = quantity AND closed_tick <= $1
			  AND sequence < (SELECT max(sequence) FROM engine.orders)`, d.LastTick)
		if err != nil {
			return err
		}
		d.OrdersDeleted = tag.RowsAffected()
		tag, err = tx.Exec(ctx, `UPDATE engine.compaction SET through_tick = $2 WHERE through_tick = $1`, through, d.LastTick)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("compaction progress moved past tick %d concurrently", through)
		}
		return nil
	})
	return d, err == nil, err
}

// results streams the original results of ticks in (after, through].
func (j *Journal) results(ctx context.Context, after, through uint64) iter.Seq2[json.RawMessage, error] {
	return func(yield func(json.RawMessage, error) bool) {
		rows, err := j.pool.Query(ctx, `SELECT result FROM engine.tick_results WHERE tick > $1 AND tick <= $2 ORDER BY tick`, after, through)
		if err != nil {
			yield(nil, err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				yield(nil, err)
				return
			}
			if !yield(raw, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(nil, err)
		}
	}
}
