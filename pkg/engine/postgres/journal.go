// Package postgres is the PostgreSQL adapter for the market engine's
// Journal. It owns the engine schema.
package postgres

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/samcjohns/t3/internal/pgmigrate"
	"github.com/samcjohns/t3/pkg/engine"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Journal implements engine.Journal.
type Journal struct {
	pool *pgxpool.Pool
}

var _ engine.Journal = (*Journal)(nil)

// NewJournal migrates the engine schema and returns a Journal using it.
func NewJournal(ctx context.Context, pool *pgxpool.Pool) (*Journal, error) {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return nil, err
	}
	if err := pgmigrate.Migrate(ctx, pool, "engine", sub); err != nil {
		return nil, err
	}
	return &Journal{pool: pool}, nil
}

func (j *Journal) Load(ctx context.Context) (engine.Snapshot, error) {
	var s engine.Snapshot
	err := j.pool.QueryRow(ctx, `SELECT
		(SELECT coalesce(max(tick), 0) FROM engine.tick_results),
		(SELECT coalesce(max(sequence), 0) FROM engine.orders)`).Scan(&s.LastTick, &s.LastSequence)
	if err != nil {
		return s, err
	}
	rows, err := j.pool.Query(ctx, `
		SELECT id, account_id, symbol, direction, type, quantity, remaining, limit_price, max_cost, time_in_force, sequence, status
		FROM engine.orders WHERE status IN ('pending', 'resting') ORDER BY sequence`)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var o engine.Order
		var quantity, remaining int64
		var status engine.OrderStatus
		if err := rows.Scan(&o.ID, &o.AccountID, &o.Symbol, &o.Direction, &o.Type, &quantity, &remaining,
			&o.LimitPrice, &o.MaxCost, &o.TimeInForce, &o.Sequence, &status); err != nil {
			return s, err
		}
		if o.TimeInForce == engine.GTC {
			o.TimeInForce = "" // stored explicitly, but empty in memory
		}
		if status == engine.StatusPending {
			o.Quantity = quantity
			s.Pending = append(s.Pending, o)
		} else {
			o.Quantity = remaining
			s.Resting = append(s.Resting, o)
		}
	}
	return s, rows.Err()
}

func (j *Journal) AcceptOrder(ctx context.Context, o engine.Order) error {
	_, err := j.pool.Exec(ctx, `
		INSERT INTO engine.orders (id, account_id, symbol, direction, type, quantity, limit_price, max_cost, time_in_force, sequence, remaining, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $6, 'pending')`,
		o.ID, o.AccountID, o.Symbol, o.Direction, o.Type, o.Quantity, o.LimitPrice, o.MaxCost, tif(o.TimeInForce), o.Sequence)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "orders_pkey" {
		return fmt.Errorf("%w: %q", engine.ErrDuplicateOrderID, o.ID)
	}
	return err
}

func (j *Journal) CommitTick(ctx context.Context, tr engine.TickResult, updates []engine.OrderUpdate) error {
	result, err := json.Marshal(tr)
	if err != nil {
		return err
	}
	ids := make([]string, len(updates))
	remaining := make([]int64, len(updates))
	statuses := make([]string, len(updates))
	for i, u := range updates {
		ids[i], remaining[i], statuses[i] = u.ID, u.Remaining, string(u.Status)
	}
	return pgx.BeginFunc(ctx, j.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO engine.tick_results (tick, timestamp, result) VALUES ($1, $2, $3)`,
			tr.Tick, tr.Timestamp, result); err != nil {
			return err
		}
		if len(updates) == 0 {
			return nil
		}
		tag, err := tx.Exec(ctx, `
			UPDATE engine.orders o
			SET remaining = u.remaining,
			    status = u.status,
			    closed_tick = CASE WHEN u.status IN ('filled', 'expired') THEN $4::bigint END
			FROM unnest($1::text[], $2::bigint[], $3::text[]) AS u(id, remaining, status)
			WHERE o.id = u.id AND o.status IN ('pending', 'resting')`,
			ids, remaining, statuses, tr.Tick)
		if err != nil {
			return err
		}
		if int(tag.RowsAffected()) != len(updates) {
			return fmt.Errorf("tick %d updated %d of %d open orders", tr.Tick, tag.RowsAffected(), len(updates))
		}
		return nil
	})
}

func (j *Journal) TicksAfter(ctx context.Context, after uint64, limit int) ([]engine.TickResult, error) {
	rows, err := j.pool.Query(ctx, `SELECT result FROM engine.tick_results WHERE tick > $1 ORDER BY tick LIMIT $2`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []engine.TickResult
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var tr engine.TickResult
		if err := json.Unmarshal(raw, &tr); err != nil {
			return nil, err
		}
		out = append(out, tr)
	}
	return out, rows.Err()
}

func (j *Journal) KnownOrders(ctx context.Context, ids []string) (map[string]bool, error) {
	rows, err := j.pool.Query(ctx, `SELECT id FROM engine.orders WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	known, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(known))
	for _, id := range known {
		out[id] = true
	}
	return out, nil
}

func tif(t engine.TimeInForce) engine.TimeInForce {
	if t == "" {
		return engine.GTC
	}
	return t
}
